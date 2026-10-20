package caseimport_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	"nodule-reader/backend/internal/application/caseimport"
)

const (
	uidStudy  = "1.2.840.113619.2.55.3.604688.100.1"
	uidSeries = "1.2.840.113619.2.55.3.604688.100.2"
	uidFrame  = "1.2.840.113619.2.55.3.604688.100.3"
	ctClass   = "1.2.840.10008.5.1.4.1.1.2"
)

// fakeArchive is a scripted archive port shared by the upload-session tests. It
// records the bodies it receives and can fail at a chosen upload index.
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

// TestPrepareHonorsCancellation proves a canceled preparation uploads nothing
// and creates no case.
func TestPrepareHonorsCancellation(t *testing.T) {
	archive := &fakeArchive{}
	store := &fakeStore{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := caseimport.NewService(archive, store).Prepare(ctx, []caseimport.StagedFile{{Path: "slice", Size: 4}}, 0, 1)
	if !errors.Is(err, caseimport.ErrCanceled) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrCanceled)
	}
	if archive.uploadCount != 0 || store.calls != 0 {
		t.Fatalf("canceled prepare uploaded %d and stored %d", archive.uploadCount, store.calls)
	}
}

func TestPrepareRejectsEmptySelection(t *testing.T) {
	_, err := caseimport.NewService(&fakeArchive{}, &fakeStore{}).Prepare(context.Background(), nil, 0, 1)
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
}

// TestValidateSeriesRejectsUnsuitableMetadata keeps the suitability rules under
// test now that they are the only shared validation surface.
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
