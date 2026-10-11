// Command migrate applies the embedded LC-1 goose migrations to a PostgreSQL
// database using explicit POSTGRES_* owner settings. It is the only component
// allowed to run DDL: it never seeds business data, never starts the HTTP
// server and never reads Orthanc configuration.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"nodule-reader/backend/internal/config"
	"nodule-reader/backend/migrations"
)

const (
	pingTimeout   = 10 * time.Second
	actionTimeout = 5 * time.Minute
	usage         = "usage: migrate <up|status|version>"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	action := args[0]
	switch action {
	case "up", "status", "version":
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}

	settings, err := config.LoadPostgres()
	if err != nil {
		// config errors name the variable, never its value.
		log.Printf("migrate: configuration error: %v", err)
		return 1
	}

	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(settings.User, settings.Password),
		Host:     net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port)),
		Path:     "/" + settings.Database,
		RawQuery: url.Values{"sslmode": {"disable"}}.Encode(),
	}).String()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("migrate: %s failed: could not open the database handle", action)
		return 1
	}
	defer db.Close()

	pingCtx, cancelPing := context.WithTimeout(context.Background(), pingTimeout)
	defer cancelPing()
	if err := db.PingContext(pingCtx); err != nil {
		log.Printf("migrate: %s failed: database is not reachable", action)
		return 1
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files)
	if err != nil {
		log.Printf("migrate: %s failed: could not load embedded migrations", action)
		return 1
	}
	defer provider.Close()

	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()

	switch action {
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			log.Printf("migrate: up failed: %v", err)
			return 1
		}
		if len(results) == 0 {
			fmt.Println("no pending migrations")
			return 0
		}
		for _, result := range results {
			fmt.Printf("applied version %d\n", result.Source.Version)
		}
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			log.Printf("migrate: status failed: %v", err)
			return 1
		}
		for _, status := range statuses {
			fmt.Printf("version %d %s\n", status.Source.Version, status.State)
		}
	case "version":
		version, err := provider.GetDBVersion(ctx)
		if err != nil {
			log.Printf("migrate: version failed: %v", err)
			return 1
		}
		fmt.Printf("version %d\n", version)
	}
	return 0
}
