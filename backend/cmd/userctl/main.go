// Command userctl administers backend PIN users: it creates users, lists them,
// rotates their PINs and revokes them. It is the only place a generated PIN is
// ever printed, and it prints each PIN exactly once. Operational errors go to
// stderr; secrets only go to stdout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"nodule-reader/backend/internal/application/auth"
	"nodule-reader/backend/internal/config"
	"nodule-reader/backend/internal/infrastructure/authcrypto"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

const (
	commandTimeout = 30 * time.Second

	usage = `usage: userctl <command> [flags]

commands:
  create --name <display-name>
  list
  rotate --id <user-id>
  revoke --id <user-id>`

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

// adminService is the user-administration surface of the auth use-case layer.
type adminService interface {
	CreateUser(ctx context.Context, displayName string) (auth.User, string, error)
	RotatePIN(ctx context.Context, userID int64) (string, error)
	RevokeUser(ctx context.Context, userID int64) error
	ListUsers(ctx context.Context) ([]auth.User, error)
}

// connectFunc opens the administration service and returns its cleanup.
type connectFunc func(ctx context.Context) (adminService, func(), error)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, connect))
}

// connect builds the production service from the auth-only configuration. The
// pool is created lazily, so connection failures surface on first command use.
func connect(ctx context.Context) (adminService, func(), error) {
	settings, err := config.LoadAuth()
	if err != nil {
		return nil, nil, err
	}
	credentials, err := authcrypto.NewCredentials(settings.PINPepper)
	if err != nil {
		return nil, nil, err
	}
	pool, err := postgres.NewPool(ctx, settings.Postgres)
	if err != nil {
		return nil, nil, err
	}
	return auth.NewService(postgres.NewAuthStore(pool), credentials), pool.Close, nil
}

// run parses and executes one command, returning the process exit code.
func run(args []string, stdout, stderr io.Writer, connect connectFunc) int {
	if len(args) == 0 {
		return usageError(stderr, "a command is required")
	}
	command, rest := args[0], args[1:]
	switch command {
	case "create":
		return runCreate(rest, stdout, stderr, connect)
	case "list":
		return runList(rest, stdout, stderr, connect)
	case "rotate":
		return runRotate(rest, stdout, stderr, connect)
	case "revoke":
		return runRevoke(rest, stdout, stderr, connect)
	default:
		return usageError(stderr, fmt.Sprintf("unknown command %q", command))
	}
}

func runCreate(args []string, stdout, stderr io.Writer, connect connectFunc) int {
	flags := newFlagSet("create")
	name := flags.String("name", "", "display name")
	if err := flags.Parse(args); err != nil {
		return usageError(stderr, err.Error())
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "create accepts no positional arguments")
	}
	if *name == "" {
		return usageError(stderr, "create requires --name")
	}

	service, ctx, closeService, code := openService(stderr, connect)
	if service == nil {
		return code
	}
	defer closeService()

	user, pin, err := service.CreateUser(ctx, *name)
	if err != nil {
		return serviceError(stderr, err)
	}
	fmt.Fprintf(stdout, "user_id: %d\ndisplay_name: %s\npin: %s\n", user.ID, user.DisplayName, pin)
	return exitOK
}

func runList(args []string, stdout, stderr io.Writer, connect connectFunc) int {
	if len(args) != 0 {
		return usageError(stderr, "list accepts no arguments")
	}

	service, ctx, closeService, code := openService(stderr, connect)
	if service == nil {
		return code
	}
	defer closeService()

	users, err := service.ListUsers(ctx)
	if err != nil {
		return serviceError(stderr, err)
	}
	printUsers(stdout, users)
	return exitOK
}

func runRotate(args []string, stdout, stderr io.Writer, connect connectFunc) int {
	userID, code := parseUserID("rotate", args, stderr)
	if code != exitOK {
		return code
	}

	service, ctx, closeService, code := openService(stderr, connect)
	if service == nil {
		return code
	}
	defer closeService()

	pin, err := service.RotatePIN(ctx, userID)
	if err != nil {
		return serviceError(stderr, err)
	}
	fmt.Fprintf(stdout, "user_id: %d\npin: %s\n", userID, pin)
	return exitOK
}

func runRevoke(args []string, stdout, stderr io.Writer, connect connectFunc) int {
	userID, code := parseUserID("revoke", args, stderr)
	if code != exitOK {
		return code
	}

	service, ctx, closeService, code := openService(stderr, connect)
	if service == nil {
		return code
	}
	defer closeService()

	if err := service.RevokeUser(ctx, userID); err != nil {
		return serviceError(stderr, err)
	}
	fmt.Fprintf(stdout, "user_id: %d\nstatus: revoked\n", userID)
	return exitOK
}

// parseUserID strictly parses --id and requires a positive identifier.
func parseUserID(command string, args []string, stderr io.Writer) (int64, int) {
	flags := newFlagSet(command)
	id := flags.Int64("id", 0, "user id")
	if err := flags.Parse(args); err != nil {
		return 0, usageError(stderr, err.Error())
	}
	if flags.NArg() != 0 {
		return 0, usageError(stderr, command+" accepts no positional arguments")
	}
	if *id <= 0 {
		return 0, usageError(stderr, command+" requires a positive --id")
	}
	return *id, exitOK
}

// openService derives one bounded command context, connects, and reports
// dependency failures. A nil service means the caller must return code.
func openService(stderr io.Writer, connect connectFunc) (adminService, context.Context, func(), int) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	service, closeService, err := connect(ctx)
	if err != nil {
		cancel()
		fmt.Fprintf(stderr, "userctl: %v\n", err)
		return nil, nil, func() {}, exitFailure
	}
	cleanup := func() {
		closeService()
		cancel()
	}
	return service, ctx, cleanup, exitOK
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

// printUsers writes one machine-stable block per user, separated by a blank
// line. It never receives or prints credential material.
func printUsers(stdout io.Writer, users []auth.User) {
	for i, user := range users {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		status := "active"
		if user.DisabledAt != nil {
			status = "revoked"
		}
		fmt.Fprintf(stdout, "user_id: %d\ndisplay_name: %s\nstatus: %s\ncreated_at: %s\nupdated_at: %s\n",
			user.ID, user.DisplayName, status, formatTime(user.CreatedAt), formatTime(user.UpdatedAt))
	}
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func usageError(stderr io.Writer, message string) int {
	fmt.Fprintf(stderr, "userctl: %s\n%s\n", message, usage)
	return exitUsage
}

// serviceError maps use-case failures to fixed messages and exit codes. It
// never prints underlying storage detail.
func serviceError(stderr io.Writer, err error) int {
	switch {
	case errors.Is(err, auth.ErrInvalidInput):
		fmt.Fprintln(stderr, "userctl: invalid input")
		return exitUsage
	case errors.Is(err, auth.ErrUserNotFound):
		fmt.Fprintln(stderr, "userctl: user not found")
		return exitFailure
	case errors.Is(err, auth.ErrDependency):
		fmt.Fprintln(stderr, "userctl: authentication storage failed")
		return exitFailure
	default:
		fmt.Fprintf(stderr, "userctl: %v\n", err)
		return exitFailure
	}
}
