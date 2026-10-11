// Package caseimport implements the LC1-08 one-series import use case: it
// inventories a directory of DICOM files, streams them through the Orthanc
// archive port, validates the resulting series metadata and asks the store for
// one atomic case/job/audit creation. It depends only on the standard library
// and the ports declared here.
package caseimport

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sentinel error categories. They classify an import failure for the CLI exit
// policy; the CLI never prints an error string, only a fixed category message.
var (
	// ErrInvalidInput reports invalid arguments, files or Orthanc responses.
	ErrInvalidInput = errors.New("invalid import input")
	// ErrUnsuitableSeries reports metadata that is not a supported CT series.
	ErrUnsuitableSeries = errors.New("unsuitable series")
	// ErrDuplicateSeries reports that the series already exists as a case.
	ErrDuplicateSeries = errors.New("series already imported")
	// ErrDependency reports an Orthanc, filesystem, network or database failure.
	ErrDependency = errors.New("dependency failure")
	// ErrCanceled reports that the caller canceled the import.
	ErrCanceled = errors.New("import canceled")
	// ErrCommitOutcomeUnknown reports that the final commit may or may not have
	// been applied because the connection was lost while committing.
	ErrCommitOutcomeUnknown = errors.New("commit outcome unknown")
)

// ReasonError attaches a fixed, non-identifying reason to a category sentinel.
// The reason is developer-facing only: never place a path, UID, credential,
// response body or raw transport error in it, because it can reach a log.
type ReasonError struct {
	sentinel error
	reason   string
}

// Fail builds a categorized error with a fixed reason.
func Fail(sentinel error, reason string) error {
	return &ReasonError{sentinel: sentinel, reason: reason}
}

func (e *ReasonError) Error() string { return e.reason }

// Unwrap exposes the category sentinel to errors.Is.
func (e *ReasonError) Unwrap() error { return e.sentinel }

// Request is one import invocation.
type Request struct {
	// Dir is the directory holding exactly the DICOM files of one series.
	Dir string
	// Source is the non-identifying label stored on the case.
	Source string
}

// Result reports the committed case and how many instances were imported.
type Result struct {
	CaseID        int64
	InstanceCount int
}

// ImportedCase is the validated case metadata handed to the persistence
// boundary. It carries only the fields LC1-07 stores; no patient tags.
type ImportedCase struct {
	Source              string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	OrthancSeriesID     string
	FrameOfReferenceUID string
	InstanceCount       int
}

// UploadedInstance is one accepted Orthanc upload response.
type UploadedInstance struct {
	ID            string
	SeriesID      string
	StudyID       string
	AlreadyStored bool
}

// Archive is the consumer-owned Orthanc port. Upload streams one DICOM file and
// returns the stored instance; DescribeSeries returns the series metadata of
// every instance; ListSeriesInstances returns the current instance inventory
// for the final recheck.
type Archive interface {
	Upload(ctx context.Context, body io.Reader, size int64) (UploadedInstance, error)
	DescribeSeries(ctx context.Context, seriesID string) (SeriesMetadata, error)
	ListSeriesInstances(ctx context.Context, seriesID string) ([]string, error)
}

// Store is the single persistence boundary for a validated import.
type Store interface {
	CreateImportedCase(ctx context.Context, input ImportedCase) (int64, error)
}

// Service orchestrates one single-series import: inventory, streamed upload,
// metadata suitability and the atomic persistence call.
type Service struct {
	archive Archive
	store   Store
}

// NewService builds the import use case from its ports.
func NewService(archive Archive, store Store) *Service {
	return &Service{archive: archive, store: store}
}

// ValidateSourceLabel trims and validates the non-identifying source label.
func ValidateSourceLabel(label string) (string, error) {
	trimmed := strings.TrimSpace(label)
	if trimmed == "" {
		return "", Fail(ErrInvalidInput, "source label is empty")
	}
	if len(trimmed) > 64 {
		return "", Fail(ErrInvalidInput, "source label is longer than 64 characters")
	}
	for _, r := range trimmed {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !valid {
			return "", Fail(ErrInvalidInput, "source label contains unsupported characters")
		}
	}
	return trimmed, nil
}

// Import runs the whole import. The caller's context is preserved across every
// upload, metadata request and the persistence call.
func (s *Service) Import(ctx context.Context, request Request) (Result, error) {
	source, err := ValidateSourceLabel(request.Source)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, Fail(ErrCanceled, "import canceled before start")
	}

	files, err := inventoryFiles(request.Dir)
	if err != nil {
		return Result{}, err
	}

	uploaded := make([]UploadedInstance, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			return Result{}, Fail(ErrCanceled, "import canceled during upload")
		}
		handle, err := openUnchanged(file)
		if err != nil {
			return Result{}, err
		}
		instance, uploadErr := s.archive.Upload(ctx, handle, file.size)
		closeErr := handle.Close()
		if uploadErr != nil {
			return Result{}, uploadErr
		}
		if closeErr != nil {
			return Result{}, Fail(ErrDependency, "cannot close an input file")
		}
		if _, duplicate := seen[instance.ID]; duplicate {
			return Result{}, Fail(ErrInvalidInput, "orthanc returned a duplicate instance identifier")
		}
		seen[instance.ID] = struct{}{}
		uploaded = append(uploaded, instance)
	}

	seriesID, studyID, err := commonParent(uploaded)
	if err != nil {
		return Result{}, err
	}

	metadata, err := s.archive.DescribeSeries(ctx, seriesID)
	if err != nil {
		return Result{}, err
	}
	if metadata.SeriesID != seriesID || metadata.StudyID != studyID {
		return Result{}, Fail(ErrUnsuitableSeries, "series metadata does not match the uploaded parents")
	}

	imported, err := ValidateSeries(metadata, uploaded)
	if err != nil {
		return Result{}, err
	}
	imported.Source = source

	inventory, err := s.archive.ListSeriesInstances(ctx, seriesID)
	if err != nil {
		return Result{}, err
	}
	if !sameInstanceSet(inventory, seen) {
		return Result{}, Fail(ErrDependency, "series inventory changed during import")
	}

	if ctx.Err() != nil {
		return Result{}, Fail(ErrCanceled, "import canceled before persistence")
	}
	caseID, err := s.store.CreateImportedCase(ctx, imported)
	if err != nil {
		return Result{}, err
	}
	return Result{CaseID: caseID, InstanceCount: imported.InstanceCount}, nil
}

// commonParent returns the shared parent identifiers of every accepted upload.
func commonParent(uploaded []UploadedInstance) (string, string, error) {
	if len(uploaded) == 0 {
		return "", "", Fail(ErrInvalidInput, "no instances were uploaded")
	}
	seriesID, studyID := uploaded[0].SeriesID, uploaded[0].StudyID
	if seriesID == "" || studyID == "" {
		return "", "", Fail(ErrInvalidInput, "upload reply lacks a parent identifier")
	}
	for _, instance := range uploaded[1:] {
		if instance.SeriesID != seriesID || instance.StudyID != studyID {
			return "", "", Fail(ErrInvalidInput, "uploads span multiple series or studies")
		}
	}
	return seriesID, studyID, nil
}

func sameInstanceSet(inventory []string, seen map[string]struct{}) bool {
	if len(inventory) != len(seen) {
		return false
	}
	for _, id := range inventory {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

// inputFile is one inventoried file: its path and the size observed at
// inventory time.
type inputFile struct {
	path string
	size int64
}

// inventoryFiles walks dir recursively and returns every regular, non-empty
// file in deterministic lexical order. Symlinks and other non-regular entries
// are rejected rather than skipped.
func inventoryFiles(dir string) ([]inputFile, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, Fail(ErrInvalidInput, "input directory is not accessible")
	}
	if !info.IsDir() {
		return nil, Fail(ErrInvalidInput, "input path is not a directory")
	}

	var files []inputFile
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return Fail(ErrInvalidInput, "input directory cannot be traversed")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return Fail(ErrInvalidInput, "input directory contains a non-regular entry")
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return Fail(ErrInvalidInput, "input file cannot be inspected")
		}
		if entryInfo.Size() == 0 {
			return Fail(ErrInvalidInput, "input file is empty")
		}
		files = append(files, inputFile{path: path, size: entryInfo.Size()})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(files) < 2 {
		return nil, Fail(ErrInvalidInput, "at least two DICOM files are required")
	}
	sort.Slice(files, func(i, j int) bool {
		return filepath.ToSlash(files[i].path) < filepath.ToSlash(files[j].path)
	})
	return files, nil
}

// openUnchanged opens an inventoried file after confirming it is still the same
// regular, non-empty file that was inventoried.
func openUnchanged(file inputFile) (*os.File, error) {
	before, err := os.Lstat(file.path)
	if err != nil {
		return nil, Fail(ErrInvalidInput, "input file disappeared")
	}
	if !before.Mode().IsRegular() {
		return nil, Fail(ErrInvalidInput, "input entry is no longer a regular file")
	}
	if before.Size() != file.size {
		return nil, Fail(ErrInvalidInput, "input file changed size during import")
	}
	handle, err := os.Open(file.path)
	if err != nil {
		return nil, Fail(ErrDependency, "cannot open an input file")
	}
	opened, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, Fail(ErrDependency, "cannot inspect an open input file")
	}
	if !opened.Mode().IsRegular() || opened.Size() != file.size {
		handle.Close()
		return nil, Fail(ErrInvalidInput, "input file changed during import")
	}
	return handle, nil
}
