package caseimport_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
)

const (
	seriesA = "series-a"
	seriesB = "series-b"
	studyA  = "study-a"
	studyB  = "study-b"
	uidSetA = "1.2.840.113619.2.55.3.604688.200.1"
	uidSetB = "1.2.840.113619.2.55.3.604688.200.9"
)

// stagedFile writes one file into a temp directory and describes it the way the
// upload intake would.
func stagedFile(t *testing.T, content string) caseimport.StagedFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slice")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write staged file: %v", err)
	}
	return caseimport.StagedFile{Path: path, Size: int64(len(content))}
}

// sliceFixture builds a valid single-frame CT series and the matching upload
// replies for the given instance identifiers.
func sliceFixture(t *testing.T, seriesID, studyID, seriesUID string, ids []string) (caseimport.SeriesMetadata, []caseimport.UploadedInstance) {
	t.Helper()
	meta := caseimport.SeriesMetadata{
		SeriesID:          seriesID,
		StudyID:           studyID,
		StudyInstanceUID:  uidStudy,
		SeriesInstanceUID: seriesUID,
	}
	uploaded := make([]caseimport.UploadedInstance, 0, len(ids))
	for i, id := range ids {
		meta.Instances = append(meta.Instances, caseimport.InstanceMetadata{
			ID:                      id,
			FrameOfReferenceUID:     uidFrame,
			SOPInstanceUID:          fmt.Sprintf("%s.%d", seriesUID, i+1),
			SOPClassUID:             ctClass,
			Modality:                "CT",
			ImageType:               []string{"ORIGINAL", "PRIMARY", "AXIAL"},
			Rows:                    512,
			Columns:                 512,
			PixelSpacing:            [2]float64{0.698, 0.698},
			ImageOrientationPatient: [6]float64{1, 0, 0, 0, 1, 0},
			ImagePositionPatient:    [3]float64{0, 0, float64(2 * i)},
			SliceThickness:          2,
		})
		uploaded = append(uploaded, caseimport.UploadedInstance{ID: id, SeriesID: seriesID, StudyID: studyID})
	}
	return meta, uploaded
}

func files(t *testing.T, count int) []caseimport.StagedFile {
	t.Helper()
	out := make([]caseimport.StagedFile, 0, count)
	for i := range count {
		out = append(out, stagedFile(t, fmt.Sprintf("dicom-%d", i)))
	}
	return out
}

func TestPrepareGroupsSeriesIntoOptions(t *testing.T) {
	metaA, upA := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2"})
	metaB, upB := sliceFixture(t, seriesB, studyB, uidSetB, []string{"id-b1", "id-b2"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{upA[0], upA[1], upB[0], upB[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: metaA, seriesB: metaB},
	}
	store := &fakeStore{}
	service := caseimport.NewService(archive, store)

	session, err := service.Prepare(context.Background(), files(t, 4), 2)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("prepare must not create a case, created %d", store.calls)
	}
	if len(session.Series) != 2 {
		t.Fatalf("want 2 series options, got %d", len(session.Series))
	}
	for i, option := range session.Series {
		if !option.Suitable || option.ReasonCode != "" {
			t.Fatalf("option %d should be suitable: %+v", i, option)
		}
		if option.InstanceCount != 2 || option.Modality != "CT" || option.Index != i+1 {
			t.Fatalf("option %d metadata wrong: %+v", i, option)
		}
	}
	if session.SkippedFiles != 2 || session.CaseID != 0 || session.SelectedSeriesID != "" {
		t.Fatalf("session not redacted/initial: %+v", session)
	}
	if session.Series[0].ID == "" || session.Series[0].ID == session.Series[1].ID {
		t.Fatal("series keys must be distinct non-empty opaque tokens")
	}
}

func TestPrepareMarksUnusableSeries(t *testing.T) {
	meta, up := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2"})
	meta.Instances[1].NumberOfFrames = 4
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{up[0], up[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: meta},
	}
	service := caseimport.NewService(archive, &fakeStore{})

	session, err := service.Prepare(context.Background(), files(t, 2), 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(session.Series) != 1 || session.Series[0].Suitable || session.Series[0].ReasonCode != caseimport.ReasonMultiFrame {
		t.Fatalf("multiframe series should be unsuitable: %+v", session.Series)
	}
}

func TestPrepareMarksSingleSliceUnsuitable(t *testing.T) {
	meta, up := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{up[0]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: meta},
	}
	service := caseimport.NewService(archive, &fakeStore{})

	session, err := service.Prepare(context.Background(), files(t, 1), 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(session.Series) != 1 || session.Series[0].Suitable || session.Series[0].ReasonCode != caseimport.ReasonSingleSlice {
		t.Fatalf("single-slice series should be unsuitable: %+v", session.Series)
	}
}

func TestPrepareRejectsUnsupportedFile(t *testing.T) {
	archive := &fakeArchive{failAt: 1, failWith: caseimport.Fail(caseimport.ErrInvalidInput, "orthanc upload rejected with status 400")}
	service := caseimport.NewService(archive, &fakeStore{})
	if _, err := service.Prepare(context.Background(), files(t, 2), 0); !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestPrepareIsSingleFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	metaA, upA := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{upA[0], upA[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: metaA},
		uploadHook: func(call int) {
			if call == 1 {
				close(started)
				<-release
			}
		},
	}
	service := caseimport.NewService(archive, &fakeStore{})

	done := make(chan error, 1)
	go func() {
		_, err := service.Prepare(context.Background(), files(t, 2), 0)
		done <- err
	}()
	<-started
	if _, err := service.Prepare(context.Background(), files(t, 2), 0); !errors.Is(err, caseimport.ErrBusy) {
		t.Fatalf("want ErrBusy during an in-flight prepare, got %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first prepare failed: %v", err)
	}
}

func TestPrepareEnforcesSessionCapacity(t *testing.T) {
	metaA, upA := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{upA[0], upA[1], upA[0], upA[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: metaA},
	}
	service := caseimport.NewServiceWithLimits(archive, &fakeStore{}, caseimport.Limits{MaxSessions: 1, MaxSeries: 64, SessionTTL: time.Minute})

	if _, err := service.Prepare(context.Background(), files(t, 2), 0); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if _, err := service.Prepare(context.Background(), files(t, 2), 0); !errors.Is(err, caseimport.ErrCapacity) {
		t.Fatalf("want ErrCapacity, got %v", err)
	}
}

func preparedSession(t *testing.T) (*caseimport.Service, *fakeArchive, *fakeStore, caseimport.Session) {
	t.Helper()
	metaA, upA := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{upA[0], upA[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: metaA},
	}
	store := &fakeStore{result: 55}
	service := caseimport.NewService(archive, store)
	session, err := service.Prepare(context.Background(), files(t, 2), 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return service, archive, store, session
}

func TestConfirmCreatesOneCaseAndIsIdempotent(t *testing.T) {
	service, _, store, session := preparedSession(t)
	key := session.Series[0].ID

	first, err := service.Confirm(context.Background(), session.ID, key)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if first.CaseID != 55 || first.Existing || first.InstanceCount != 2 {
		t.Fatalf("unexpected result: %+v", first)
	}
	second, err := service.Confirm(context.Background(), session.ID, key)
	if err != nil {
		t.Fatalf("repeat confirm: %v", err)
	}
	if second != first {
		t.Fatalf("repeat confirm changed the result: %+v vs %+v", second, first)
	}
	if store.calls != 1 {
		t.Fatalf("want exactly one case creation, got %d", store.calls)
	}
}

func TestConfirmRejectsAnotherSeriesAfterConfirm(t *testing.T) {
	service, _, _, session := preparedSession(t)
	if _, err := service.Confirm(context.Background(), session.ID, session.Series[0].ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := service.Confirm(context.Background(), session.ID, "some-other-key"); !errors.Is(err, caseimport.ErrAlreadyConfirmed) {
		t.Fatalf("want ErrAlreadyConfirmed, got %v", err)
	}
}

func TestConfirmReturnsExistingCase(t *testing.T) {
	service, _, store, session := preparedSession(t)
	store.existingFound = true
	store.existingID = 91

	result, err := service.Confirm(context.Background(), session.ID, session.Series[0].ID)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !result.Existing || result.CaseID != 91 {
		t.Fatalf("want existing case 91, got %+v", result)
	}
	if store.calls != 0 {
		t.Fatalf("must not create a duplicate case, created %d", store.calls)
	}
}

func TestConfirmResolvesDuplicateUniqueViolation(t *testing.T) {
	service, _, store, session := preparedSession(t)
	store.failure = caseimport.Fail(caseimport.ErrDuplicateSeries, "series already imported")
	store.existingFound = true
	store.existingID = 12

	result, err := service.Confirm(context.Background(), session.ID, session.Series[0].ID)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !result.Existing || result.CaseID != 12 {
		t.Fatalf("want existing case 12, got %+v", result)
	}
}

func TestConfirmRejectsUnknownKeyAndUpload(t *testing.T) {
	service, _, _, session := preparedSession(t)
	if _, err := service.Confirm(context.Background(), session.ID, "no-such-key"); !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
	if _, err := service.Confirm(context.Background(), "no-such-upload", session.Series[0].ID); !errors.Is(err, caseimport.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetReconcilesUncertainConfirm(t *testing.T) {
	service, _, store, session := preparedSession(t)
	store.failure = caseimport.Fail(caseimport.ErrCommitOutcomeUnknown, "commit outcome unknown")

	if _, err := service.Confirm(context.Background(), session.ID, session.Series[0].ID); !errors.Is(err, caseimport.ErrCommitOutcomeUnknown) {
		t.Fatalf("want ErrCommitOutcomeUnknown, got %v", err)
	}
	view, err := service.Get(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if view.CaseID != 0 || view.SelectedSeriesID != "" {
		t.Fatalf("session must stay unconfirmed after an unknown commit: %+v", view)
	}

	// The case is now visible: the reconciliation must complete the session.
	store.failure = nil
	store.existingFound = true
	store.existingID = 77
	view, err = service.Get(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("reconcile get: %v", err)
	}
	if view.CaseID != 77 || view.SelectedSeriesID != session.Series[0].ID {
		t.Fatalf("reconciliation did not complete the session: %+v", view)
	}
}

func TestSessionExpires(t *testing.T) {
	metaA, upA := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{upA[0], upA[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: metaA},
	}
	service := caseimport.NewServiceWithLimits(archive, &fakeStore{}, caseimport.Limits{SessionTTL: 5 * time.Millisecond, MaxSessions: 8, MaxSeries: 64})
	session, err := service.Prepare(context.Background(), files(t, 2), 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := service.Get(context.Background(), session.ID); !errors.Is(err, caseimport.ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestDiscardRemovesSession(t *testing.T) {
	service, _, _, session := preparedSession(t)
	if err := service.Discard(context.Background(), session.ID); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := service.Get(context.Background(), session.ID); !errors.Is(err, caseimport.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestConfirmRefusesSubsetOfExistingSeries(t *testing.T) {
	// The series in Orthanc has three instances but only two were uploaded:
	// the exact-set check must refuse rather than merge.
	metaA, upA := sliceFixture(t, seriesA, studyA, uidSetA, []string{"id-a1", "id-a2", "id-a3"})
	archive := &fakeArchive{
		results:          []caseimport.UploadedInstance{upA[0], upA[1]},
		metadataBySeries: map[string]caseimport.SeriesMetadata{seriesA: metaA},
	}
	service := caseimport.NewService(archive, &fakeStore{})
	session, err := service.Prepare(context.Background(), files(t, 2), 0)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if session.Series[0].Suitable {
		t.Fatal("a subset of an existing series must not be suitable")
	}
}
