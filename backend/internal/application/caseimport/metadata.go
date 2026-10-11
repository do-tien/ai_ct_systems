package caseimport

import (
	"math"
	"sort"
	"strings"
)

// InstanceMetadata is the metadata-only view of one stored instance. Every
// numeric and array field is present and finite by the time the archive port
// returns it; the adapter rejects absent, wrong-typed or non-finite values.
// NumberOfFrames is 0 when the tag is absent.
type InstanceMetadata struct {
	ID                      string
	FrameOfReferenceUID     string
	SOPInstanceUID          string
	SOPClassUID             string
	Modality                string
	ImageType               []string
	NumberOfFrames          int
	Rows                    int
	Columns                 int
	PixelSpacing            [2]float64
	ImageOrientationPatient [6]float64
	ImagePositionPatient    [3]float64
	SliceThickness          float64
}

// SeriesMetadata is the metadata-only view of one Orthanc series: its
// identifiers plus the per-instance metadata used for the suitability checks.
type SeriesMetadata struct {
	SeriesID          string
	StudyID           string
	StudyInstanceUID  string
	SeriesInstanceUID string
	Instances         []InstanceMetadata
}

const (
	ctSOPClassUID       = "1.2.840.10008.5.1.4.1.1.2"
	spacingToleranceMM  = 0.01
	directionTolerance  = 1e-5
	maxSliceThicknessMM = 2.5
	maxSliceStepMM      = 2.5
)

// ValidateSeries establishes whether the described series is a supported
// conventional single-frame CT series and returns the metadata to persist. Any
// failure is an ErrUnsuitableSeries: no partially validated case is produced.
func ValidateSeries(metadata SeriesMetadata, uploaded []UploadedInstance) (ImportedCase, error) {
	unsuitable := func() (ImportedCase, error) {
		return ImportedCase{}, Fail(ErrUnsuitableSeries, "series is not a supported single-frame CT series")
	}
	if len(uploaded) < 2 || len(metadata.Instances) < 2 {
		return unsuitable()
	}
	if !validUID(metadata.StudyInstanceUID) || !validUID(metadata.SeriesInstanceUID) {
		return unsuitable()
	}
	if metadata.SeriesID == "" || metadata.StudyID == "" {
		return unsuitable()
	}
	for _, instance := range uploaded {
		if instance.SeriesID != metadata.SeriesID || instance.StudyID != metadata.StudyID {
			return unsuitable()
		}
	}

	// The series must contain exactly the submitted instances.
	uploadedIDs := make(map[string]struct{}, len(uploaded))
	for _, instance := range uploaded {
		uploadedIDs[instance.ID] = struct{}{}
	}
	if len(uploadedIDs) != len(uploaded) {
		return unsuitable()
	}
	describedIDs := make(map[string]struct{}, len(metadata.Instances))
	for _, instance := range metadata.Instances {
		describedIDs[instance.ID] = struct{}{}
	}
	if len(describedIDs) != len(metadata.Instances) || len(describedIDs) != len(uploadedIDs) {
		return unsuitable()
	}
	for id := range describedIDs {
		if _, ok := uploadedIDs[id]; !ok {
			return unsuitable()
		}
	}

	frameUID := metadata.Instances[0].FrameOfReferenceUID
	rows := metadata.Instances[0].Rows
	columns := metadata.Instances[0].Columns
	spacing := metadata.Instances[0].PixelSpacing
	thickness := metadata.Instances[0].SliceThickness
	orientation := metadata.Instances[0].ImageOrientationPatient
	sopUIDs := make(map[string]struct{}, len(metadata.Instances))

	for _, instance := range metadata.Instances {
		if !validUID(instance.FrameOfReferenceUID) || instance.FrameOfReferenceUID != frameUID {
			return unsuitable()
		}
		if !validUID(instance.SOPInstanceUID) {
			return unsuitable()
		}
		if _, duplicate := sopUIDs[instance.SOPInstanceUID]; duplicate {
			return unsuitable()
		}
		sopUIDs[instance.SOPInstanceUID] = struct{}{}
		if trimPadding(instance.Modality) != "CT" {
			return unsuitable()
		}
		if trimPadding(instance.SOPClassUID) != ctSOPClassUID {
			return unsuitable()
		}
		if instance.NumberOfFrames > 1 {
			return unsuitable()
		}
		if !imageTypeUsable(instance.ImageType) {
			return unsuitable()
		}
		if instance.Rows <= 0 || instance.Columns <= 0 || instance.Rows != rows || instance.Columns != columns {
			return unsuitable()
		}
		if !validSpacing(instance.PixelSpacing) || !closeEnough(instance.PixelSpacing[:], spacing[:], spacingToleranceMM) {
			return unsuitable()
		}
		if !(instance.SliceThickness > 0 && instance.SliceThickness <= maxSliceThicknessMM) ||
			abs(instance.SliceThickness-thickness) > spacingToleranceMM {
			return unsuitable()
		}
		if !validOrientation(instance.ImageOrientationPatient) ||
			!closeEnough(instance.ImageOrientationPatient[:], orientation[:], directionTolerance) {
			return unsuitable()
		}
		if !finiteValues(instance.ImagePositionPatient[:]...) {
			return unsuitable()
		}
	}

	normal := cross(orientation[:3], orientation[3:])
	if norm(normal) < 0.5 {
		return unsuitable()
	}

	type slice struct {
		position float64
		originMM [3]float64
	}
	slices := make([]slice, 0, len(metadata.Instances))
	for _, instance := range metadata.Instances {
		slices = append(slices, slice{
			position: dot(instance.ImagePositionPatient[:], normal),
			originMM: instance.ImagePositionPatient,
		})
	}
	sort.Slice(slices, func(i, j int) bool { return slices[i].position < slices[j].position })

	steps := make([]float64, 0, len(slices)-1)
	for i := 1; i < len(slices); i++ {
		step := slices[i].position - slices[i-1].position
		if step <= 0 {
			return unsuitable()
		}
		steps = append(steps, step)
	}
	median := medianOf(steps)
	for i := 1; i < len(slices); i++ {
		step := slices[i].position - slices[i-1].position
		if step > maxSliceStepMM {
			return unsuitable()
		}
		if abs(step-median) > maxFloat(spacingToleranceMM, median*0.01) {
			return unsuitable()
		}
		expected := scale(normal, step)
		residual := sub(sub(slices[i].originMM[:], slices[i-1].originMM[:]), expected)
		if norm(residual) > spacingToleranceMM {
			return unsuitable()
		}
	}

	return ImportedCase{
		StudyInstanceUID:    metadata.StudyInstanceUID,
		SeriesInstanceUID:   metadata.SeriesInstanceUID,
		OrthancSeriesID:     metadata.SeriesID,
		FrameOfReferenceUID: frameUID,
		InstanceCount:       len(metadata.Instances),
	}, nil
}

// trimPadding removes DICOM value padding and surrounding whitespace.
func trimPadding(value string) string {
	return strings.Trim(value, "\x00 ")
}

// validUID reports whether value is a syntactically valid DICOM UID: dot
// separated decimal components, at most 64 characters, no redundant leading
// zeros.
func validUID(value string) bool {
	uid := trimPadding(value)
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, component := range strings.Split(uid, ".") {
		if component == "" {
			return false
		}
		if len(component) > 1 && component[0] == '0' {
			return false
		}
		for _, r := range component {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// imageTypeUsable rejects localizers and scouts, which share the CT SOP class
// but are not the diagnostic series this importer accepts.
func imageTypeUsable(components []string) bool {
	if len(components) == 0 {
		return false
	}
	for _, component := range components {
		switch stripped := strings.ToUpper(trimPadding(component)); stripped {
		case "":
			return false
		case "LOCALIZER", "SCOUT":
			return false
		}
	}
	return true
}

func validSpacing(spacing [2]float64) bool {
	return finiteValues(spacing[0], spacing[1]) && spacing[0] > 0 && spacing[1] > 0
}

func validOrientation(iop [6]float64) bool {
	if !finiteValues(iop[:]...) {
		return false
	}
	row := iop[:3]
	column := iop[3:]
	return abs(norm(row)-1) <= directionTolerance &&
		abs(norm(column)-1) <= directionTolerance &&
		abs(dot(row, column)) <= directionTolerance
}

func closeEnough(a, b []float64, tolerance float64) bool {
	for i := range a {
		if abs(a[i]-b[i]) > tolerance {
			return false
		}
	}
	return true
}

func finiteValues(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func medianOf(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func cross(a, b []float64) []float64 {
	return []float64{
		a[1]*b[2] - a[2]*b[1],
		a[2]*b[0] - a[0]*b[2],
		a[0]*b[1] - a[1]*b[0],
	}
}

func dot(a, b []float64) float64 {
	return a[0]*b[0] + a[1]*b[1] + a[2]*b[2]
}

func norm(v []float64) float64 {
	return math.Sqrt(dot(v, v))
}

func scale(v []float64, factor float64) []float64 {
	return []float64{v[0] * factor, v[1] * factor, v[2] * factor}
}

func sub(a, b []float64) []float64 {
	return []float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]}
}
