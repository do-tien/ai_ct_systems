package caseimport_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
)

const (
	uidStudy  = "1.2.840.113619.2.55.3.604688.100.1"
	uidSeries = "1.2.840.113619.2.55.3.604688.100.2"
	uidFrame  = "1.2.840.113619.2.55.3.604688.100.3"
	ctClass   = "1.2.840.10008.5.1.4.1.1.2"
)

// fakeArchive is a scripted archive port. It records the bodies it receives,
// keeps the readers so a test can prove the service closed them, and can fail
// at a chosen upload index.
type fakeArchive struct {
	uploadCount  int
	failAt       int
	failWith     error
	bodies       [][]byte
	readers      []io.Reader
	describeFunc func() (caseimport.SeriesMetadata, error)
	listFunc     func() ([]string, error)
	describeCall int
	listCall     int
	metadata     caseimport.SeriesMetadata
	// results, when set, are returned by Upload in call order.
	results []caseimport.UploadedInstance
	// metadataBySeries, when set, is returned by DescribeSeries by series id.
	metadataBySeries map[string]caseimport.SeriesMetadata
	// uploadHook runs at the start of every Upload call.
	uploadHook func(call int)
}

func (f *fakeArchive) Upload(ctx context.Context, body io.Reader, size int64) (caseimport.UploadedInstance, error) {
	f.uploadCount++
	if f.uploadHook != nil {
		f.uploadHook(f.uploadCount)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return caseimport.UploadedInstance{}, err
	}
	f.bodies = append(f.bodies, raw)
	f.readers = append(f.readers, body)
	if f.failAt > 0 && f.uploadCount == f.failAt {
		if f.failWith != nil {
			return caseimport.UploadedInstance{}, f.failWith
		}
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrDependency, "scripted upload failure")
	}
	if err := ctx.Err(); err != nil {
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrCanceled, "scripted cancel")
	}
	if len(f.results) >= f.uploadCount {
		return f.results[f.uploadCount-1], nil
	}
	return caseimport.UploadedInstance{
		ID:       instanceIDAt(f.uploadCount - 1),
		SeriesID: orthancSeriesID,
		StudyID:  orthancStudyID,
	}, nil
}

func (f *fakeArchive) DescribeSeries(ctx context.Context, seriesID string) (caseimport.SeriesMetadata, error) {
	f.describeCall++
	if f.describeFunc != nil {
		return f.describeFunc()
	}
	if f.metadataBySeries != nil {
		if meta, ok := f.metadataBySeries[seriesID]; ok {
			return meta, nil
		}
		return caseimport.SeriesMetadata{}, caseimport.Fail(caseimport.ErrDependency, "unknown scripted series")
	}
	return f.metadata, nil
}

func (f *fakeArchive) ListSeriesInstances(ctx context.Context, seriesID string) ([]string, error) {
	f.listCall++
	if f.listFunc != nil {
		return f.listFunc()
	}
	source := f.metadata
	if f.metadataBySeries != nil {
		if meta, ok := f.metadataBySeries[seriesID]; ok {
			source = meta
		}
	}
	ids := make([]string, 0, len(source.Instances))
	for _, instance := range source.Instances {
		ids = append(ids, instance.ID)
	}
	return ids, nil
}

type fakeStore struct {
	calls   int
	last    caseimport.ImportedCase
	result  int64
	failure error
	// existing mirrors FindCaseBySeriesUID's answer.
	existingID    int64
	existingFound bool
	lookupErr     error
	lookups       int
}

func (f *fakeStore) FindCaseBySeriesUID(ctx context.Context, uid string) (int64, bool, error) {
	f.lookups++
	if f.lookupErr != nil {
		return 0, false, f.lookupErr
	}
	if f.existingFound {
		return f.existingID, true, nil
	}
	return 0, false, nil
}

func (f *fakeStore) CreateImportedCase(ctx context.Context, input caseimport.ImportedCase) (int64, error) {
	f.calls++
	f.last = input
	if f.failure != nil {
		return 0, f.failure
	}
	if f.result == 0 {
		return 42, nil
	}
	return f.result, nil
}

const (
	orthancSeriesID = "11111111-22222222-33333333-44444444-55555555"
	orthancStudyID  = "66666666-77777777-88888888-99999999-00000000"
)

func instanceIDAt(index int) string {
	return fmt.Sprintf("%08d-00000000-00000000-00000000-00000000", index+1)
}

// validMetadata builds a three-instance single-frame CT series with 2 mm
// spacing along z and distinct SOP Instance UIDs.
func validMetadata() (caseimport.SeriesMetadata, []caseimport.UploadedInstance) {
	return validMetadataCount(3)
}

// validMetadataCount builds a single-frame CT series with count instances.
func validMetadataCount(count int) (caseimport.SeriesMetadata, []caseimport.UploadedInstance) {
	metadata := caseimport.SeriesMetadata{
		SeriesID:          orthancSeriesID,
		StudyID:           orthancStudyID,
		StudyInstanceUID:  uidStudy,
		SeriesInstanceUID: uidSeries,
	}
	uploaded := make([]caseimport.UploadedInstance, 0, count)
	for i := range count {
		id := instanceIDAt(i)
		metadata.Instances = append(metadata.Instances, caseimport.InstanceMetadata{
			ID:                      id,
			FrameOfReferenceUID:     uidFrame,
			SOPInstanceUID:          fmt.Sprintf("1.2.840.10008.5.1.4.1.1.2.%d", i+1),
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
		uploaded = append(uploaded, caseimport.UploadedInstance{ID: id, SeriesID: orthancSeriesID, StudyID: orthancStudyID})
	}
	return metadata, uploaded
}

func writeTestFiles(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestImportUploadsFilesInStableOrder(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{
		"z-last":  []byte("zzz"),
		"a/third": []byte("ccc"),
		"m.dcm":   []byte("mmm"),
		"a/aa":    []byte("bbb"),
	})
	metadata, _ := validMetadataCount(4)
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{}
	service := caseimport.NewService(archive, store)

	result, err := service.Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.CaseID != 42 || result.InstanceCount != 4 {
		t.Fatalf("result = %+v", result)
	}
	want := [][]byte{[]byte("bbb"), []byte("ccc"), []byte("mmm"), []byte("zzz")}
	if len(archive.bodies) != len(want) {
		t.Fatalf("uploaded %d files, want %d", len(archive.bodies), len(want))
	}
	for i := range want {
		if !bytes.Equal(archive.bodies[i], want[i]) {
			t.Fatalf("upload %d = %q, want %q", i, archive.bodies[i], want[i])
		}
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
	if store.last.Source != "local" || store.last.InstanceCount != 4 ||
		store.last.SeriesInstanceUID != uidSeries || store.last.OrthancSeriesID != orthancSeriesID ||
		store.last.FrameOfReferenceUID != uidFrame {
		t.Fatalf("stored case = %+v", store.last)
	}
}

func TestImportReversedFileNamesKeepIdentifiers(t *testing.T) {
	metadata, _ := validMetadata()

	first := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b"), "c": []byte("c")})
	second := writeTestFiles(t, map[string][]byte{"c": []byte("c"), "b": []byte("b"), "a": []byte("a")})

	run := func(dir string) caseimport.Result {
		t.Helper()
		archive := &fakeArchive{metadata: metadata}
		store := &fakeStore{}
		result, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "LIDC-IDRI"})
		if err != nil {
			t.Fatalf("Import(%s): %v", dir, err)
		}
		return result
	}
	one, two := run(first), run(second)
	if one != two {
		t.Fatalf("results differ: %+v vs %+v", one, two)
	}
}

func TestImportClosesEveryOpenedFile(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadataCount(2)
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{}
	if _, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	for i, reader := range archive.readers {
		if _, err := reader.Read(make([]byte, 1)); err == nil {
			t.Fatalf("reader %d still open after Upload returned", i)
		}
	}
}

func TestImportRejectsUnusableDirectories(t *testing.T) {
	oneFile := writeTestFiles(t, map[string][]byte{"only": []byte("data")})
	emptyFile := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": {}})
	emptyDir := t.TempDir()
	metadata, _ := validMetadata()

	cases := []struct {
		name string
		dir  string
	}{
		{"empty_directory", emptyDir},
		{"single_file", oneFile},
		{"zero_byte_file", emptyFile},
		{"missing_directory", filepath.Join(emptyDir, "does-not-exist")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := &fakeArchive{metadata: metadata}
			store := &fakeStore{}
			_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: tc.dir, Source: "local"})
			if !errors.Is(err, caseimport.ErrInvalidInput) {
				t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
			}
			if store.calls != 0 {
				t.Fatalf("store called %d times", store.calls)
			}
		})
	}
}

func TestImportRejectsSymlink(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"real": []byte("data"), "real2": []byte("data2")})
	if err := os.Symlink(filepath.Join(dir, "real2"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("cannot create symlink on this host: %v", err)
	}
	metadata, _ := validMetadata()
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

func TestImportRejectsFileChangedAfterInventory(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "z": []byte("z")})
	metadata, _ := validMetadata()

	started := make(chan struct{})
	release := make(chan struct{})
	archive := &fakeArchive{metadata: metadata}
	fake := &blockingArchive{fakeArchive: archive, started: started, release: release}
	store := &fakeStore{}

	done := make(chan error, 1)
	go func() {
		_, err := caseimport.NewService(fake, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
		done <- err
	}()

	<-started
	if err := os.WriteFile(filepath.Join(dir, "z"), []byte("z-much-longer"), 0o644); err != nil {
		t.Fatalf("grow file: %v", err)
	}
	close(release)

	err := <-done
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

// blockingArchive blocks the first upload until the test releases it.
type blockingArchive struct {
	*fakeArchive
	started chan struct{}
	release chan struct{}
}

func (b *blockingArchive) Upload(ctx context.Context, body io.Reader, size int64) (caseimport.UploadedInstance, error) {
	if b.uploadCount == 0 {
		close(b.started)
		<-b.release
	}
	return b.fakeArchive.Upload(ctx, body, size)
}

func TestImportStopsOnUploadFailure(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b"), "c": []byte("c")})
	metadata, _ := validMetadata()
	archive := &fakeArchive{metadata: metadata, failAt: 2}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
	if archive.uploadCount != 2 {
		t.Fatalf("upload count = %d, want 2", archive.uploadCount)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

func TestImportRejectsDuplicateUploadedIDs(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadata()
	archive := &duplicateIDArchive{fakeArchive: &fakeArchive{metadata: metadata}}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

// duplicateIDArchive returns the same instance ID for every upload.
type duplicateIDArchive struct {
	*fakeArchive
}

func (d *duplicateIDArchive) Upload(ctx context.Context, body io.Reader, size int64) (caseimport.UploadedInstance, error) {
	instance, err := d.fakeArchive.Upload(ctx, body, size)
	if err != nil {
		return instance, err
	}
	instance.ID = instanceIDAt(0)
	return instance, nil
}

func TestImportRequiresExactInstanceSet(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b"), "c": []byte("c")})
	fresh := func() caseimport.SeriesMetadata {
		metadata, _ := validMetadata()
		return metadata
	}

	cases := []struct {
		name   string
		mutate func(caseimport.SeriesMetadata) caseimport.SeriesMetadata
		want   error
	}{
		{"substituted_id", func(m caseimport.SeriesMetadata) caseimport.SeriesMetadata {
			m.Instances[1].ID = "99999999-99999999-99999999-99999999-99999999"
			return m
		}, caseimport.ErrUnsuitableSeries},
		{"missing_instance", func(m caseimport.SeriesMetadata) caseimport.SeriesMetadata {
			m.Instances = m.Instances[:2]
			return m
		}, caseimport.ErrUnsuitableSeries},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata := tc.mutate(fresh())
			archive := &fakeArchive{metadata: metadata}
			store := &fakeStore{}
			_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if store.calls != 0 {
				t.Fatalf("store called %d times", store.calls)
			}
		})
	}

	t.Run("extra_preexisting_instance", func(t *testing.T) {
		metadata := fresh()
		metadata.Instances = append(metadata.Instances, caseimport.InstanceMetadata{
			ID: "77777777-77777777-77777777-77777777-77777777", FrameOfReferenceUID: uidFrame,
			SOPInstanceUID: "1.2.840.10008.5.1.4.1.1.2.9", SOPClassUID: ctClass, Modality: "CT",
			ImageType: []string{"ORIGINAL"}, Rows: 512, Columns: 512, PixelSpacing: [2]float64{0.698, 0.698},
			ImageOrientationPatient: [6]float64{1, 0, 0, 0, 1, 0}, ImagePositionPatient: [3]float64{0, 0, 8}, SliceThickness: 2,
		})
		archive := &fakeArchive{metadata: metadata}
		store := &fakeStore{}
		_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
		if !errors.Is(err, caseimport.ErrUnsuitableSeries) {
			t.Fatalf("error = %v, want %v", err, caseimport.ErrUnsuitableSeries)
		}
		if store.calls != 0 {
			t.Fatalf("store called %d times", store.calls)
		}
	})
}

func TestImportRejectsChangedFinalInventory(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b"), "c": []byte("c")})
	metadata, _ := validMetadata()
	archive := &fakeArchive{metadata: metadata}
	archive.listFunc = func() ([]string, error) {
		return []string{instanceIDAt(0), instanceIDAt(1), "55555555-55555555-55555555-55555555-55555555"}, nil
	}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

func TestImportPropagatesDuplicateSeriesFromStore(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadataCount(2)
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{failure: caseimport.ErrDuplicateSeries}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrDuplicateSeries) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDuplicateSeries)
	}
}

func TestImportHonorsCancellation(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadata()
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := caseimport.NewService(archive, store).Import(ctx, caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrCanceled) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrCanceled)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

func TestImportRejectsInvalidSource(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadata()
	for _, source := range []string{"", "  ", "has space", strings.Repeat("x", 65), "semi;colon"} {
		archive := &fakeArchive{metadata: metadata}
		store := &fakeStore{}
		_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: source})
		if !errors.Is(err, caseimport.ErrInvalidInput) {
			t.Fatalf("source %q: error = %v, want %v", source, err, caseimport.ErrInvalidInput)
		}
		if archive.uploadCount != 0 {
			t.Fatalf("source %q: uploads = %d, want 0", source, archive.uploadCount)
		}
	}
}

// Ensure the scripted store never receives a partially built case when the
// series is unsuitable.
func TestValidateSeriesRejectsUnsuitableMetadata(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*caseimport.SeriesMetadata)
	}{
		{"zero_iop", func(m *caseimport.SeriesMetadata) { m.Instances[0].ImageOrientationPatient = [6]float64{} }}, {"mixed_frame_uid", func(m *caseimport.SeriesMetadata) { m.Instances[1].FrameOfReferenceUID = "1.2.3.999" }},
		{"duplicate_sop", func(m *caseimport.SeriesMetadata) { m.Instances[1].SOPInstanceUID = m.Instances[0].SOPInstanceUID }},
		{"duplicate_instance", func(m *caseimport.SeriesMetadata) { m.Instances[1].ID = m.Instances[0].ID }},
		{"non_ct_modality", func(m *caseimport.SeriesMetadata) { m.Instances[1].Modality = "MR" }},
		{"seg_object", func(m *caseimport.SeriesMetadata) { m.Instances[1].SOPClassUID = "1.2.840.10008.5.1.4.1.1.66.4" }},
		{"localizer", func(m *caseimport.SeriesMetadata) {
			m.Instances[1].ImageType = []string{"ORIGINAL", "LOCALIZER"}
		}},
		{"empty_image_type", func(m *caseimport.SeriesMetadata) { m.Instances[1].ImageType = []string{""} }},
		{"blank_image_type_component", func(m *caseimport.SeriesMetadata) {
			m.Instances[1].ImageType = []string{"ORIGINAL", " "}
		}},
		{"scout", func(m *caseimport.SeriesMetadata) { m.Instances[1].ImageType = []string{"SCOUT"} }},
		{"multiframe", func(m *caseimport.SeriesMetadata) { m.Instances[1].NumberOfFrames = 4 }},
		{"missing_frame_uid", func(m *caseimport.SeriesMetadata) { m.Instances[1].FrameOfReferenceUID = "" }},
		{"malformed_uid", func(m *caseimport.SeriesMetadata) { m.Instances[1].SOPInstanceUID = "1..2" }},
		{"leading_zero_uid", func(m *caseimport.SeriesMetadata) { m.Instances[1].SOPInstanceUID = "1.02.3" }},
		{"zero_spacing", func(m *caseimport.SeriesMetadata) { m.Instances[1].PixelSpacing = [2]float64{0, 0.698} }},
		{"nonfinite_spacing", func(m *caseimport.SeriesMetadata) { m.Instances[1].PixelSpacing = [2]float64{math.NaN(), 0.698} }},
		{"inconsistent_spacing", func(m *caseimport.SeriesMetadata) { m.Instances[1].PixelSpacing = [2]float64{0.798, 0.698} }},
		{"inconsistent_columns", func(m *caseimport.SeriesMetadata) { m.Instances[1].Columns = 256 }},
		{"zero_rows", func(m *caseimport.SeriesMetadata) { m.Instances[1].Rows = 0 }},
		{"nonorthogonal_iop", func(m *caseimport.SeriesMetadata) {
			m.Instances[0].ImageOrientationPatient = [6]float64{1, 0, 0, 0.1, 1, 0}
		}},
		{"inconsistent_iop", func(m *caseimport.SeriesMetadata) {
			m.Instances[1].ImageOrientationPatient = [6]float64{1, 0, 0, 0, 1, 0.0001}
		}},
		{"thick_slices", func(m *caseimport.SeriesMetadata) {
			for i := range m.Instances {
				m.Instances[i].SliceThickness = 3.0
			}
		}},
		{"duplicate_position", func(m *caseimport.SeriesMetadata) {
			m.Instances[2].ImagePositionPatient = m.Instances[1].ImagePositionPatient
		}},
		{"gap_between_slices", func(m *caseimport.SeriesMetadata) {
			m.Instances[2].ImagePositionPatient = [3]float64{0, 0, 8}
		}},
		{"irregular_spacing", func(m *caseimport.SeriesMetadata) {
			// 0, 2, 2.1 mm: step 0.1 mm deviates from the median 2 mm.
			m.Instances[2].ImagePositionPatient = [3]float64{0, 0, 2.1}
		}},
		{"in_plane_drift", func(m *caseimport.SeriesMetadata) {
			m.Instances[1].ImagePositionPatient = [3]float64{0.5, 0, 2}
		}},
		{"mixed_series_id", func(m *caseimport.SeriesMetadata) { m.SeriesID = "99999999-99999999-99999999-99999999-99999999" }},
		{"malformed_series_uid", func(m *caseimport.SeriesMetadata) { m.SeriesInstanceUID = "abc" }},
		{"single_instance", func(m *caseimport.SeriesMetadata) { m.Instances = m.Instances[:1] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadata, uploaded := validMetadata()
			tc.mutate(&metadata)
			_, err := caseimport.ValidateSeries(metadata, uploaded)
			if !errors.Is(err, caseimport.ErrUnsuitableSeries) {
				t.Fatalf("error = %v, want %v", err, caseimport.ErrUnsuitableSeries)
			}
		})
	}
}

func TestValidateSeriesAcceptsReorderedAndNegativePositions(t *testing.T) {
	metadata, uploaded := validMetadata()
	// Reverse the instance order and place the series at negative LPS z.
	for i := range metadata.Instances {
		metadata.Instances[i].ImagePositionPatient = [3]float64{0, 0, float64(-2 * i)}
	}
	for i, j := 0, len(metadata.Instances)-1; i < j; i, j = i+1, j-1 {
		metadata.Instances[i], metadata.Instances[j] = metadata.Instances[j], metadata.Instances[i]
	}
	imported, err := caseimport.ValidateSeries(metadata, uploaded)
	if err != nil {
		t.Fatalf("ValidateSeries: %v", err)
	}
	if imported.InstanceCount != 3 || imported.FrameOfReferenceUID != uidFrame || imported.SeriesInstanceUID != uidSeries {
		t.Fatalf("imported = %+v", imported)
	}
}

func TestImportStopsWhenStoreFails(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b"), "c": []byte("c")})
	metadata, _ := validMetadata()
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{failure: caseimport.Fail(caseimport.ErrCommitOutcomeUnknown, "connection lost during commit")}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrCommitOutcomeUnknown) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrCommitOutcomeUnknown)
	}
}

func TestImportStopsWhenDescribeFails(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	archive := &fakeArchive{describeFunc: func() (caseimport.SeriesMetadata, error) {
		return caseimport.SeriesMetadata{}, caseimport.Fail(caseimport.ErrDependency, "orthanc metadata request failed")
	}}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

func TestImportRejectsUploadsSpanningStudies(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadataCount(2)
	archive := &mixedParentArchive{fakeArchive: &fakeArchive{metadata: metadata}}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

// mixedParentArchive returns a different parent study for every other upload.
type mixedParentArchive struct {
	*fakeArchive
}

func (m *mixedParentArchive) Upload(ctx context.Context, body io.Reader, size int64) (caseimport.UploadedInstance, error) {
	instance, err := m.fakeArchive.Upload(ctx, body, size)
	if err != nil {
		return instance, err
	}
	if m.uploadCount%2 == 0 {
		instance.StudyID = "99999999-99999999-99999999-99999999-99999999"
	}
	return instance, nil
}

func TestImportRejectsMetadataParentMismatch(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadataCount(2)
	metadata.StudyID = "99999999-99999999-99999999-99999999-99999999"
	archive := &fakeArchive{metadata: metadata}
	store := &fakeStore{}
	_, err := caseimport.NewService(archive, store).Import(context.Background(), caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrUnsuitableSeries) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrUnsuitableSeries)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

func TestImportDeadlineDoesNotPersist(t *testing.T) {
	dir := writeTestFiles(t, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	metadata, _ := validMetadata()
	archive := &slowArchive{fakeArchive: &fakeArchive{metadata: metadata}}
	store := &fakeStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := caseimport.NewService(archive, store).Import(ctx, caseimport.Request{Dir: dir, Source: "local"})
	if !errors.Is(err, caseimport.ErrCanceled) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrCanceled)
	}
	if store.calls != 0 {
		t.Fatalf("store called %d times", store.calls)
	}
}

// slowArchive delays the first upload until the caller's context expires.
type slowArchive struct {
	*fakeArchive
}

func (s *slowArchive) Upload(ctx context.Context, body io.Reader, size int64) (caseimport.UploadedInstance, error) {
	select {
	case <-ctx.Done():
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrCanceled, "upload canceled")
	case <-time.After(2 * time.Second):
	}
	return s.fakeArchive.Upload(ctx, body, size)
}
