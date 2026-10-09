package httpapi

import (
	"encoding/json"
	"net/http"

	"nodule-reader/backend/internal/application/health"
)

const (
	statusOK          = "ok"
	statusDegraded    = "degraded"
	statusUnavailable = "unavailable"
)

type healthChecks struct {
	Backend  string `json:"backend"`
	Postgres string `json:"postgres"`
	Orthanc  string `json:"orthanc"`
}

type healthResponse struct {
	Status string       `json:"status"`
	Checks healthChecks `json:"checks"`
}

type healthHandler struct {
	health *health.Service
}

// ServeHTTP maps the health result to the wire contract: HTTP 200 when every
// dependency is reachable, otherwise 503 with each failing dependency marked
// unavailable. The body never carries error text, credentials, internal URLs,
// AI data, or patient data.
func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	result := h.health.Check(r.Context())

	checks := healthChecks{Backend: statusOK, Postgres: statusOK, Orthanc: statusOK}
	status := statusOK
	code := http.StatusOK
	if !result.PostgresAvailable {
		checks.Postgres = statusUnavailable
		status = statusDegraded
		code = http.StatusServiceUnavailable
	}
	if !result.OrthancAvailable {
		checks.Orthanc = statusUnavailable
		status = statusDegraded
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(healthResponse{Status: status, Checks: checks})
}
