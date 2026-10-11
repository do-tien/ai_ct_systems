// Command importer imports exactly one CT series into Orthanc and creates one
// case with one queued AI job. It is a manual-DI composition root: it reads
// configuration once, builds the concrete adapters, and owns the process exit
// policy. No HTTP server starts and no public upload API exists.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/config"
	"nodule-reader/backend/internal/infrastructure/orthanc"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

const (
	// importDeadline bounds the whole command, including Ctrl+C cancellation.
	importDeadline = 15 * time.Minute
	// pingTimeout bounds the pre-flight PostgreSQL reachability check.
	pingTimeout = 5 * time.Second

	exitOK          = 0
	exitFailure     = 1
	exitInvalidArgs = 2
)

const usage = `usage: importer --dir <DICOM directory> [--source <label>]

Imports exactly one conventional single-frame CT series into Orthanc and
creates one case with one queued AI job.

  --dir     directory containing only the DICOM files of one series (required)
  --source  non-identifying provenance label, at most 64 characters
            (default: local)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run performs the whole command and returns the documented exit code. The
// caller's context carries cancellation; it is never replaced by Background.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("importer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("dir", "", "directory containing one DICOM series")
	source := flags.String("source", "local", "non-identifying source label")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		return invalidArguments(stderr)
	}
	if strings.TrimSpace(*dir) == "" {
		return invalidArguments(stderr)
	}
	label, err := caseimport.ValidateSourceLabel(*source)
	if err != nil {
		return invalidArguments(stderr)
	}

	ctx, cancel := context.WithTimeout(ctx, importDeadline)
	defer cancel()

	postgresSettings, err := config.LoadPostgres()
	if err != nil {
		fmt.Fprintf(stderr, "importer failed: invalid configuration: %s\n", err)
		return exitFailure
	}
	orthancSettings, err := config.LoadOrthanc()
	if err != nil {
		fmt.Fprintf(stderr, "importer failed: invalid configuration: %s\n", err)
		return exitFailure
	}

	pool, err := postgres.NewPool(ctx, postgresSettings)
	if err != nil {
		fmt.Fprintln(stderr, "importer failed: cannot configure the database pool")
		return exitFailure
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(ctx, pingTimeout)
	pingErr := pool.Ping(pingCtx)
	pingCancel()
	if pingErr != nil {
		fmt.Fprintln(stderr, "importer failed: database is unreachable")
		return exitFailure
	}

	httpClient := orthanc.NewImportHTTPClient()
	defer httpClient.CloseIdleConnections()
	archive := orthanc.NewClient(httpClient, orthancSettings.URL, orthancSettings.User, orthancSettings.Password)
	service := caseimport.NewService(archive, postgres.NewImportStore(pool))

	started := time.Now()
	result, err := service.Import(ctx, caseimport.Request{Dir: *dir, Source: label})
	if err != nil {
		fmt.Fprintf(stderr, "importer failed: %s\n", categoryMessage(err))
		return exitCode(err)
	}
	elapsed := time.Since(started)

	fmt.Fprintf(stdout, "imported case_id=%d instances=%d elapsed_s=%.3f\n",
		result.CaseID, result.InstanceCount, elapsed.Seconds())
	return exitOK
}

func invalidArguments(stderr io.Writer) int {
	fmt.Fprint(stderr, usage)
	return exitInvalidArgs
}

// exitCode maps an application error category to the documented exit code.
// Nothing from the error itself is printed.
func exitCode(err error) int {
	switch {
	case errors.Is(err, caseimport.ErrInvalidInput),
		errors.Is(err, caseimport.ErrUnsuitableSeries),
		errors.Is(err, caseimport.ErrDuplicateSeries):
		return exitInvalidArgs
	default:
		return exitFailure
	}
}

// categoryMessage returns the fixed, safe message for an error category.
func categoryMessage(err error) string {
	switch {
	case errors.Is(err, caseimport.ErrInvalidInput):
		return "invalid input: check the directory contents and the source label"
	case errors.Is(err, caseimport.ErrUnsuitableSeries):
		return "unsuitable series: the directory is not a supported single-frame CT series"
	case errors.Is(err, caseimport.ErrDuplicateSeries):
		return "duplicate series: a case for this series already exists"
	case errors.Is(err, caseimport.ErrCommitOutcomeUnknown):
		return "commit outcome unknown: reconcile the case by series UID before rerunning"
	case errors.Is(err, caseimport.ErrCanceled):
		return "canceled: the import did not complete"
	default:
		return "dependency failure: check Orthanc and PostgreSQL availability"
	}
}
