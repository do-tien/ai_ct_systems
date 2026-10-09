// Package httpapi exposes the backend's HTTP transport.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"nodule-reader/backend/internal/application/health"
)

// NewRouter builds the HTTP handler around the health use case.
func NewRouter(healthService *health.Service) http.Handler {
	router := chi.NewRouter()
	router.Method(http.MethodGet, "/healthz", &healthHandler{health: healthService})
	return router
}
