package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubProbe struct {
	err            error
	calls          int
	observedCtxErr error
	delay          time.Duration
}

func (s *stubProbe) Check(ctx context.Context) error {
	s.calls++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
		}
	}
	if err := ctx.Err(); err != nil {
		s.observedCtxErr = err
		return err
	}
	return s.err
}

func TestCheckReportsEachDependencyIndependently(t *testing.T) {
	postgres := &stubProbe{err: errors.New("postgres down")}
	orthanc := &stubProbe{}

	result := NewService(postgres, orthanc).Check(context.Background())

	if result.PostgresAvailable {
		t.Error("expected postgres to be unavailable")
	}
	if !result.OrthancAvailable {
		t.Error("expected orthanc to stay available when only postgres fails")
	}
	if orthanc.calls != 1 {
		t.Errorf("expected orthanc to still be probed, got %d calls", orthanc.calls)
	}
}

func TestCheckKeepsOrthancWithinBudgetAfterPostgresTimesOut(t *testing.T) {
	postgres := &stubProbe{delay: 10 * time.Second}
	orthanc := &stubProbe{}

	start := time.Now()
	result := NewService(postgres, orthanc).Check(context.Background())
	elapsed := time.Since(start)

	if result.PostgresAvailable {
		t.Error("expected postgres to be reported unavailable after timing out")
	}
	if !result.OrthancAvailable {
		t.Error("expected orthanc to be probed and available within the remaining budget")
	}
	if postgres.observedCtxErr != context.DeadlineExceeded {
		t.Errorf("expected postgres probe deadline to be exceeded, got %v", postgres.observedCtxErr)
	}
	if elapsed >= totalTimeout+500*time.Millisecond {
		t.Errorf("check exceeded the overall budget: %v", elapsed)
	}
}

func TestCheckPropagatesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	postgres := &stubProbe{}
	orthanc := &stubProbe{}
	result := NewService(postgres, orthanc).Check(ctx)

	if postgres.observedCtxErr != context.Canceled {
		t.Errorf("expected postgres probe to observe cancellation, got %v", postgres.observedCtxErr)
	}
	if orthanc.observedCtxErr != context.Canceled {
		t.Errorf("expected orthanc probe to observe cancellation, got %v", orthanc.observedCtxErr)
	}
	if result.PostgresAvailable || result.OrthancAvailable {
		t.Error("expected both dependencies unavailable when the request is cancelled")
	}
}

func TestCheckRecoversAfterAFailedProbe(t *testing.T) {
	postgres := &stubProbe{err: errors.New("postgres down")}
	service := NewService(postgres, &stubProbe{})

	if service.Check(context.Background()).PostgresAvailable {
		t.Fatal("expected first check to report postgres unavailable")
	}

	postgres.err = nil
	if !service.Check(context.Background()).PostgresAvailable {
		t.Error("expected a later check to report postgres available again")
	}
}
