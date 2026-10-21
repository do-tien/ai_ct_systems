package postgres_test

import (
	"context"
	"errors"
	"testing"

	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

// TestImportStoreFindsCaseBySeriesUID exercises the commit-reconciliation
// lookup against real PostgreSQL: an existing series is found, an unknown one
// is reported as absent and a repeated create stays a duplicate.
func TestImportStoreFindsCaseBySeriesUID(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)
	ctx := context.Background()

	input := importInput("1.2.3.4.5.200", "1.2.3.4.5.199", "upload")
	caseID, err := store.CreateImportedCase(ctx, input)
	if err != nil {
		t.Fatalf("create imported case: %v", err)
	}

	found, ok, err := store.FindCaseBySeriesUID(ctx, input.SeriesInstanceUID)
	if err != nil || !ok || found != caseID {
		t.Fatalf("lookup existing = (%d, %v, %v), want (%d, true, nil)", found, ok, err, caseID)
	}

	if _, ok, err := store.FindCaseBySeriesUID(ctx, "1.2.3.4.5.999999"); err != nil || ok {
		t.Fatalf("lookup unknown = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	if _, _, err := store.FindCaseBySeriesUID(ctx, ""); !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("lookup empty = %v, want ErrInvalidInput", err)
	}

	// A second create for the same series is the unique-constraint path the
	// confirm flow resolves by looking the case up again.
	if _, err := store.CreateImportedCase(ctx, input); !errors.Is(err, caseimport.ErrDuplicateSeries) {
		t.Fatalf("duplicate create = %v, want ErrDuplicateSeries", err)
	}
	found, ok, err = store.FindCaseBySeriesUID(ctx, input.SeriesInstanceUID)
	if err != nil || !ok || found != caseID {
		t.Fatalf("lookup after duplicate = (%d, %v, %v), want (%d, true, nil)", found, ok, err, caseID)
	}
}
