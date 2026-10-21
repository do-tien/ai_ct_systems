// Package httpapi exposes the backend's HTTP transport.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/health"
	"nodule-reader/backend/internal/application/initialread"
)

// NewRouter builds the HTTP handler around the health use case, the
// independent-read API, the post-lock AI/final API, the DICOM upload endpoints
// and the read-only DICOMweb proxy gate. Every dependency is required: the
// composition root always injects the real upload handler and the real gate
// wrapping the Orthanc proxy, never a disabled or placeholder route.
func NewRouter(healthService *health.Service, initialReadService *initialread.Service, finalReadService *finalread.Service, uploadHandler http.Handler, dicomwebHandler http.Handler) http.Handler {
	router := chi.NewRouter()
	router.Method(http.MethodGet, "/healthz", &healthHandler{health: healthService})

	initialRead := &initialReadHandler{service: initialReadService}
	router.Get("/api/cases/{id}", initialRead.getCase)
	router.Get("/api/cases/{id}/reads/initial", initialRead.getInitial)
	router.Put("/api/cases/{id}/reads/initial/marks", initialRead.putMarks)
	router.Post("/api/cases/{id}/reads/initial/submit", initialRead.submit)

	finalRead := &finalReadHandler{service: finalReadService}
	router.Get("/api/cases/{id}/ai", finalRead.getAI)
	router.Get("/api/cases/{id}/reads/final", finalRead.getFinal)
	router.Put("/api/cases/{id}/reads/final", finalRead.putFinal)
	router.Post("/api/cases/{id}/finalize", finalRead.finalize)
	// Register the upload namespace with and without a trailing wildcard so both
	// "/api/uploads" and "/api/uploads/..." reach the upload handler, which
	// inspects the unnormalized request path itself. The handler only accepts
	// the documented method/path pairs.
	router.Handle("/api/uploads", uploadHandler)
	router.Handle("/api/uploads/*", uploadHandler)
	// Register the namespace with and without a trailing wildcard so both
	// "/dicom-web" and "/dicom-web/..." reach the gate. The gate inspects the
	// unnormalized request path itself, so no path-cleaning middleware may be
	// added here: cleaning would turn a rejected traversal into an accepted
	// route before the gate runs.
	router.Handle("/dicom-web", dicomwebHandler)
	router.Handle("/dicom-web/*", dicomwebHandler)
	return router
}
