package python

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"nodule-reader/backend/internal/application/aijob"
)

const (
	// resultFileLimit caps the result document read from disk (4 MiB).
	resultFileLimit int64 = 4 << 20
	// processWaitDelay bounds how long a killed process may keep inherited
	// output handles open before Wait returns.
	processWaitDelay = time.Second
	// scoreThreshold is the settled LC1-01 pass threshold.
	scoreThreshold = "0.5"
)

// Settings holds the native interpreter, bundle directory and private job root.
type Settings struct {
	Python    string
	BundleDir string
	JobDir    string
	Device    string
}

// Runner executes the inference CLI once per job inside a fresh private
// workspace. It never reuses a workspace or a result file.
type Runner struct {
	settings      Settings
	archiveLimits archiveLimits
	resultLimit   int64
}

// NewRunner builds the process adapter with the documented default ceilings.
func NewRunner(settings Settings) *Runner {
	return &Runner{
		settings:      settings,
		archiveLimits: defaultArchiveLimits(),
		resultLimit:   resultFileLimit,
	}
}

// Run downloads the job's series, extracts it, runs the CLI and returns the
// process outcome. Infrastructure failures are returned as *aijob.CodeError
// with a fixed category and the sanitized stderr tail; a completed process is
// returned as an Execution whose Result is nil when no result file exists.
func (r *Runner) Run(ctx context.Context, job aijob.Job, archive aijob.Archive) (aijob.Execution, error) {
	if err := os.MkdirAll(r.settings.JobDir, 0o700); err != nil {
		return aijob.Execution{}, &aijob.CodeError{Code: aijob.CodeWorkspaceError}
	}
	workspace, err := os.MkdirTemp(r.settings.JobDir, fmt.Sprintf("job-%d-", job.ID))
	if err != nil {
		return aijob.Execution{}, &aijob.CodeError{Code: aijob.CodeWorkspaceError}
	}
	defer os.RemoveAll(workspace)

	if err := r.download(ctx, archive, job.OrthancSeriesID, filepath.Join(workspace, "archive.zip")); err != nil {
		return aijob.Execution{}, err
	}

	dicomDir := filepath.Join(workspace, "dicom")
	if err := os.Mkdir(dicomDir, 0o700); err != nil {
		return aijob.Execution{}, &aijob.CodeError{Code: aijob.CodeWorkspaceError}
	}
	if err := extractArchive(ctx, filepath.Join(workspace, "archive.zip"), dicomDir, r.archiveLimits); err != nil {
		if code := cancellationCode(ctx); code != "" {
			return aijob.Execution{}, &aijob.CodeError{Code: code}
		}
		return aijob.Execution{}, &aijob.CodeError{Code: aijob.CodeArchiveInvalid}
	}

	return r.execute(ctx, workspace, dicomDir)
}

// download streams the series archive into an exclusively created file.
func (r *Runner) download(ctx context.Context, archive aijob.Archive, seriesID, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return &aijob.CodeError{Code: aijob.CodeWorkspaceError}
	}
	defer file.Close()

	if err := archive.Download(ctx, seriesID, file); err != nil {
		if code := cancellationCode(ctx); code != "" {
			return &aijob.CodeError{Code: code}
		}
		return &aijob.CodeError{Code: aijob.CodeOrthancArchiveError}
	}
	return nil
}

// execute runs the CLI and reads the result file. Timeout or cancellation takes
// precedence over the exit status and over any file the process left behind.
func (r *Runner) execute(ctx context.Context, workspace, dicomDir string) (aijob.Execution, error) {
	resultPath := filepath.Join(workspace, "result.json")
	tail := &tailWriter{limit: aijob.MaxStderrTailBytes}

	command := exec.CommandContext(ctx, r.settings.Python,
		"-m", "nodule_ai.infer",
		"--series-dir", dicomDir,
		"--out", resultPath,
		"--bundle-dir", r.settings.BundleDir,
		"--score-threshold", scoreThreshold,
		"--device", r.settings.Device,
	)
	command.Dir = workspace
	command.Env = childEnv()
	command.Stdout = io.Discard
	command.Stderr = tail
	command.WaitDelay = processWaitDelay

	runErr := command.Run()
	stderrTail := aijob.SanitizeStderrTail(tail.String())

	if code := cancellationCode(ctx); code != "" {
		return aijob.Execution{}, &aijob.CodeError{Code: code, StderrTail: stderrTail}
	}

	exitCode := 0
	if runErr != nil {
		// A process that started always leaves a ProcessState. exec.ErrWaitDelay
		// (a spawned grandchild holding an inherited output handle past
		// WaitDelay) is still a completed process, never a start failure; only a
		// missing ProcessState means the binary could not be launched.
		if command.ProcessState == nil {
			return aijob.Execution{}, &aijob.CodeError{Code: aijob.CodeProcessStartFailed, StderrTail: stderrTail}
		}
		exitCode = command.ProcessState.ExitCode()
	}

	result, err := readResultFile(resultPath, r.resultLimit)
	if err != nil {
		return aijob.Execution{}, &aijob.CodeError{Code: aijob.CodeResultInvalid, StderrTail: stderrTail}
	}
	return aijob.Execution{ExitCode: exitCode, Result: result, StderrTail: stderrTail}, nil
}

// cancellationCode maps the context state to a fixed timeout/cancellation
// category, or "" when the context is still live.
func cancellationCode(ctx context.Context) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return aijob.CodeAITimeout
	case errors.Is(ctx.Err(), context.Canceled):
		return aijob.CodeAICanceled
	default:
		return ""
	}
}

// readResultFile returns the result document, nil when it is absent, and an
// error when it is not a fresh regular file or exceeds the cap. It never
// follows a symlink or a path replaced after the check.
func readResultFile(path string, limit int64) (json.RawMessage, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, errArchiveInvalid
	}
	if !info.Mode().IsRegular() {
		return nil, errArchiveInvalid
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, errArchiveInvalid
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errArchiveInvalid
	}

	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, errArchiveInvalid
	}
	if int64(len(data)) > limit {
		return nil, errArchiveInvalid
	}
	return json.RawMessage(data), nil
}

// tailWriter keeps only the final limit bytes written to it.
type tailWriter struct {
	buffer []byte
	limit  int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buffer = append(w.buffer, p...)
	if len(w.buffer) > w.limit {
		w.buffer = w.buffer[len(w.buffer)-w.limit:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	return string(w.buffer)
}

// childEnvAllowed lists the environment names the child process may inherit.
// Everything else, including PostgreSQL and Orthanc credentials, is dropped.
var childEnvAllowed = map[string]bool{
	"PATH": true, "PATHEXT": true, "SYSTEMROOT": true, "SYSTEMDRIVE": true,
	"WINDIR": true, "COMSPEC": true, "TEMP": true, "TMP": true, "TMPDIR": true,
	"USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true, "HOME": true,
	"NUMBER_OF_PROCESSORS": true, "PROCESSOR_ARCHITECTURE": true, "OS": true,
	"TZ": true, "LANG": true, "LC_ALL": true,
}

// childEnv returns only the allowlisted platform and interpreter essentials.
func childEnv() []string {
	source := os.Environ()
	out := make([]string, 0, len(source))
	for _, entry := range source {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if childEnvAllowed[strings.ToUpper(name)] {
			out = append(out, entry)
		}
	}
	return out
}
