package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	statusOK          = "ok"
	statusDegraded    = "degraded"
	statusUnavailable = "unavailable"

	totalTimeout   = 3 * time.Second
	dbTimeout      = 1 * time.Second
	orthancTimeout = 2 * time.Second
	maxBodyBytes   = 64 * 1024
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
	pool            *pgxpool.Pool
	orthancClient   *http.Client
	orthancURL      string
	orthancUser     string
	orthancPassword string
}

// ServeHTTP checks each dependency with its own deadline inside an overall
// three-second budget, so a hanging database cannot skip the Orthanc check.
// The response never carries patient data, AI results, credentials, internal
// URLs, or raw error text.
func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), totalTimeout)
	defer cancel()

	checks := healthChecks{Backend: statusOK}

	dbCtx, dbCancel := context.WithTimeout(ctx, dbTimeout)
	if err := h.pool.Ping(dbCtx); err != nil {
		checks.Postgres = statusUnavailable
	} else {
		checks.Postgres = statusOK
	}
	dbCancel()

	orthancCtx, orthancCancel := context.WithTimeout(ctx, orthancTimeout)
	if h.orthancAvailable(orthancCtx) {
		checks.Orthanc = statusOK
	} else {
		checks.Orthanc = statusUnavailable
	}
	orthancCancel()

	status := statusOK
	code := http.StatusOK
	if checks.Postgres != statusOK || checks.Orthanc != statusOK {
		status = statusDegraded
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(healthResponse{Status: status, Checks: checks})
}

func (h *healthHandler) orthancAvailable(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, h.orthancURL+"/system", nil)
	if err != nil {
		return false
	}
	request.SetBasicAuth(h.orthancUser, h.orthancPassword)

	response, err := h.orthancClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return false
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return false
	}

	var system struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(body, &system); err != nil {
		return false
	}
	return system.Version != ""
}
