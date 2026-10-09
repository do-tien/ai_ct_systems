// Command server runs the nodule-reader backend: a chi HTTP server exposing a
// connection-aware health endpoint for the browser-facing frontend.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/api"
	"nodule-reader/backend/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(1)
	}

	pool, err := newPool(context.Background(), cfg)
	if err != nil {
		log.Printf("postgres configuration error: %v", err)
		os.Exit(1)
	}
	defer pool.Close()

	orthancClient := &http.Client{Timeout: 2 * time.Second}
	orthancURL := strings.TrimRight(cfg.OrthancURL, "/")

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(pool, orthancClient, orthancURL, cfg.OrthancUser, cfg.OrthancPassword),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go handleShutdown(server)

	log.Printf("server listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
}

func newPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	// The base config carries no credentials; each field is assigned from
	// validated configuration. SSL is disabled because the backend and
	// PostgreSQL communicate over the local Docker network only.
	poolConfig, err := pgxpool.ParseConfig("postgres://localhost:5432/postgres?sslmode=disable")
	if err != nil {
		return nil, err
	}
	poolConfig.ConnConfig.Host = cfg.PostgresHost
	poolConfig.ConnConfig.Port = uint16(cfg.PostgresPort)
	poolConfig.ConnConfig.Database = cfg.PostgresDB
	poolConfig.ConnConfig.User = cfg.PostgresUser
	poolConfig.ConnConfig.Password = cfg.PostgresPassword
	poolConfig.MaxConns = 4
	return pgxpool.NewWithConfig(ctx, poolConfig)
}

func handleShutdown(server *http.Server) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals

	log.Printf("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
