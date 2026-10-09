// Command server runs the nodule-reader backend: a chi HTTP server exposing a
// connection-aware health endpoint for the browser-facing frontend. It is the
// composition root: it reads configuration once, builds the concrete
// dependencies, wires them, and owns process lifecycle and cleanup.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nodule-reader/backend/internal/application/health"
	"nodule-reader/backend/internal/config"
	"nodule-reader/backend/internal/infrastructure/orthanc"
	"nodule-reader/backend/internal/infrastructure/postgres"
	httpapi "nodule-reader/backend/internal/transport/http"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Printf("configuration error: %v", err)
		os.Exit(1)
	}

	pool, err := postgres.NewPool(context.Background(), postgres.Settings{
		Host:     cfg.PostgresHost,
		Port:     cfg.PostgresPort,
		Database: cfg.PostgresDB,
		User:     cfg.PostgresUser,
		Password: cfg.PostgresPassword,
	})
	if err != nil {
		log.Printf("postgres configuration error: %v", err)
		os.Exit(1)
	}
	defer pool.Close()

	orthancClient := orthanc.NewClient(
		&http.Client{Timeout: 2 * time.Second},
		cfg.OrthancURL,
		cfg.OrthancUser,
		cfg.OrthancPassword,
	)

	healthService := health.NewService(postgres.NewPoolProbe(pool), orthancClient)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(healthService),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go handleShutdown(server)

	log.Printf("server listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
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
