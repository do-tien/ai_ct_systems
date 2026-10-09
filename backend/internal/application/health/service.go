// Package health implements the operational health use case: it probes the
// backend's dependencies and reports which are currently reachable.
package health

import (
	"context"
	"time"
)

const (
	totalTimeout    = 3 * time.Second
	postgresTimeout = 1 * time.Second
	orthancTimeout  = 2 * time.Second
)

// Probe reports whether one dependency is currently reachable.
type Probe interface {
	Check(ctx context.Context) error
}

// Result reports per-dependency availability for a single health check.
type Result struct {
	PostgresAvailable bool
	OrthancAvailable  bool
}

// Service probes the configured dependencies on demand.
type Service struct {
	postgres Probe
	orthanc  Probe
}

// NewService builds a health service from the dependency probes it owns.
func NewService(postgres Probe, orthanc Probe) *Service {
	return &Service{postgres: postgres, orthanc: orthanc}
}

// Check probes each dependency sequentially inside one overall deadline, so a
// hanging database cannot skip the Orthanc check while the budget remains. It
// inherits cancellation from the request context.
func (s *Service) Check(ctx context.Context) Result {
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel()

	return Result{
		PostgresAvailable: reachable(ctx, s.postgres, postgresTimeout),
		OrthancAvailable:  reachable(ctx, s.orthanc, orthancTimeout),
	}
}

func reachable(parent context.Context, probe Probe, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return probe.Check(ctx) == nil
}
