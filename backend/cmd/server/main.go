// Command server runs the nodule-reader backend: a chi HTTP server exposing a
// connection-aware health endpoint and a read-only DICOMweb image proxy for the
// browser-facing frontend. It is the composition root: it reads configuration
// once, builds the concrete dependencies, wires them, and owns process
// lifecycle and cleanup.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"nodule-reader/backend/internal/application/aijob"
	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/health"
	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/config"
	"nodule-reader/backend/internal/infrastructure/orthanc"
	"nodule-reader/backend/internal/infrastructure/postgres"
	"nodule-reader/backend/internal/infrastructure/python"
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

	// The health probe keeps its short-lived client; image transport is a
	// separate, longer-lived transport so a slow or large image never shares
	// the probe's two-second budget or connection pool.
	orthancClient := orthanc.NewClient(
		&http.Client{Timeout: 2 * time.Second},
		cfg.OrthancURL,
		cfg.OrthancUser,
		cfg.OrthancPassword,
	)
	healthService := health.NewService(postgres.NewPoolProbe(pool), orthancClient)

	initialReadService := initialread.NewService(postgres.NewInitialReadStore(pool))
	finalReadService := finalread.NewService(postgres.NewFinalReadStore(pool))

	// Uploads use their own bounded Orthanc client: the two-second health probe
	// and the long-lived image transport must not share a connection budget.
	uploadService := caseimport.NewService(
		orthanc.NewClient(orthanc.NewImportHTTPClient(), cfg.OrthancURL, cfg.OrthancUser, cfg.OrthancPassword),
		postgres.NewImportStore(pool),
	)

	imageTransport := newImageTransport()
	defer imageTransport.CloseIdleConnections()

	dicomwebProxy, err := orthanc.NewDICOMwebProxy(cfg.OrthancURL, cfg.OrthancUser, cfg.OrthancPassword, imageTransport)
	if err != nil {
		// Never log the underlying URL or error value: it can carry the
		// configured Orthanc authority.
		log.Printf("dicomweb proxy configuration error")
		os.Exit(1)
	}

	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(
			healthService,
			initialReadService,
			finalReadService,
			httpapi.NewUploadHandler(uploadService, cfg.UploadDir),
			httpapi.NewDICOMwebHandler(dicomwebProxy),
		),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The AI job worker starts outside the HTTP handlers and owns its own
	// lifetime: a bounded startup snapshot of queued jobs is drained one at a
	// time, so listening never waits on a database snapshot, an archive
	// download or Python startup.
	archiveClient := orthanc.NewArchiveClient(orthanc.NewArchiveHTTPClient(), cfg.OrthancURL, cfg.OrthancUser, cfg.OrthancPassword)
	runner := python.NewRunner(python.Settings{
		Python:    cfg.AIPython,
		BundleDir: cfg.AIBundleDir,
		JobDir:    cfg.AIJobDir,
		Device:    cfg.AIDevice,
	})
	jobService := aijob.NewService(postgres.NewAIJobStore(pool), archiveClient, runner, cfg.AITimeout)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		if err := jobService.Run(workerCtx); err != nil {
			log.Printf("ai job worker stopped: %v", err)
		}
	}()

	// One lifecycle owner derives shutdown from a signal or a listen failure:
	// it stops the worker first, then shuts HTTP down inside its five-second
	// budget, and the worker is joined before the pool closes.
	listenFailed := make(chan struct{})
	go supervise(stopWorker, server, listenFailed)

	log.Printf("server listening on %s", cfg.HTTPAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server error: %v", err)
		close(listenFailed)
	}
	worker.Wait()
}

// newImageTransport builds the bounded transport for DICOMweb image traffic:
// a five-second connect timeout, a thirty-second response-header timeout and
// automatic compression disabled. It performs no retries.
func newImageTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		IdleConnTimeout:       90 * time.Second,
	}
}

// supervise is the single lifecycle owner. On a termination signal or a listen
// failure it stops the AI worker first, then shuts HTTP down within the existing
// five-second budget. The main goroutine joins the worker before the database
// pool is closed.
func supervise(stopWorker func(), server *http.Server, listenFailed <-chan struct{}) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	select {
	case <-signals:
		log.Printf("shutting down")
	case <-listenFailed:
	}

	stopWorker()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
