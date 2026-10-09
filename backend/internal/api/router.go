// Package api wires the backend's HTTP routes.
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewRouter builds the HTTP handler for the backend. The pool and the Orthanc
// HTTP client are shared for the server's lifetime; the router never opens new
// connections per request.
func NewRouter(pool *pgxpool.Pool, orthancClient *http.Client, orthancURL string, orthancUser string, orthancPassword string) http.Handler {
	router := chi.NewRouter()
	router.Method(http.MethodGet, "/healthz", &healthHandler{
		pool:            pool,
		orthancClient:   orthancClient,
		orthancURL:      orthancURL,
		orthancUser:     orthancUser,
		orthancPassword: orthancPassword,
	})
	return router
}
