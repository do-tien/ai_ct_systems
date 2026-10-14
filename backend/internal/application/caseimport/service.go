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
	"sync"
	"time"
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
	// ErrBusy reports that another upload is already being prepared.
	ErrBusy = errors.New("another upload is being prepared")
	// ErrCapacity reports that the maximum number of sessions is already open.
	ErrCapacity = errors.New("too many upload sessions are open")
	// ErrExpired reports that the upload session is no longer selectable.
	ErrExpired = errors.New("upload session expired")
	// ErrAlreadyConfirmed reports a second, different selection for a session.
	ErrAlreadyConfirmed = errors.New("upload session already confirmed")
	// ErrNotFound reports an unknown upload session.
	ErrNotFound = errors.New("upload session not found")
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
// Existing is true when the series was already imported and no new case was
// created.
type Result struct {
	CaseID        int64
	InstanceCount int
	Existing      bool
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
	// FindCaseBySeriesUID returns the case already created for a validated
	// Series Instance UID, so a repeated upload returns the existing case
	// instead of creating a second one.
	FindCaseBySeriesUID(ctx context.Context, seriesInstanceUID string) (int64, bool, error)
}

// Service orchestrates the upload use case: it streams staged files through the
// Orthanc port, validates the resulting series and keeps one short-lived
// in-memory session per prepared upload until the caller confirms a series.
type Service struct {
	archive Archive
	store   Store
	source  string
	limits  Limits
	// prepare is a capacity-1 semaphore: only one upload is examined at a time.
	prepare chan struct{}
	// mu guards the session registry. It is never held across Orthanc or
	// database I/O; per-session serialization uses sessionState.mu instead.
	mu       sync.Mutex
	sessions map[string]*sessionState
}

// NewService builds the upload use case from its ports with the default
// session budgets.
func NewService(archive Archive, store Store) *Service {
	return NewServiceWithLimits(archive, store, DefaultLimits())
}

// NewServiceWithLimits builds the upload use case with explicit session
// budgets. Any zero value falls back to its default.
func NewServiceWithLimits(archive Archive, store Store, limits Limits) *Service {
	defaults := DefaultLimits()
	if limits.SessionTTL <= 0 {
		limits.SessionTTL = defaults.SessionTTL
	}
	if limits.MaxSessions <= 0 {
		limits.MaxSessions = defaults.MaxSessions
	}
	if limits.MaxSeries <= 0 {
		limits.MaxSeries = defaults.MaxSeries
	}
	return &Service{
		archive:  archive,
		store:    store,
		source:   uploadSource,
		limits:   limits,
		prepare:  make(chan struct{}, 1),
		sessions: make(map[string]*sessionState),
	}
}

// Prepare streams every staged file through the archive port, groups the
// accepted instances by series, validates each group and stores one short-lived
// session. It never creates a case and never reveals AI data. A file Orthanc
// rejects fails the whole preparation; a valid DICOM series that is not
// suitable still appears as a blocked option with a fixed reason code.
func (s *Service) Prepare(ctx context.Context, files []StagedFile, skippedFiles int) (Session, error) {
	if len(files) == 0 {
		return Session{}, Fail(ErrInvalidInput, "no files were uploaded")
	}
	if ctx.Err() != nil {
		return Session{}, Fail(ErrCanceled, "upload canceled before preparation")
	}
	select {
	case s.prepare <- struct{}{}:
		defer func() { <-s.prepare }()
	default:
		return Session{}, Fail(ErrBusy, "another upload is being prepared")
	}

	uploaded := make([]UploadedInstance, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if ctx.Err() != nil {
			return Session{}, Fail(ErrCanceled, "upload canceled during preparation")
		}
		if file.Path == "" || file.Size <= 0 {
			return Session{}, Fail(ErrInvalidInput, "a staged file is empty")
		}
		handle, err := os.Open(file.Path)
		if err != nil {
			return Session{}, Fail(ErrDependency, "cannot open a staged file")
		}
		info, statErr := handle.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			handle.Close()
			return Session{}, Fail(ErrInvalidInput, "a staged file changed during preparation")
		}
		instance, uploadErr := s.archive.Upload(ctx, handle, file.Size)
		closeErr := handle.Close()
		if uploadErr != nil {
			return Session{}, uploadErr
		}
		if closeErr != nil {
			return Session{}, Fail(ErrDependency, "cannot close a staged file")
		}
		if _, duplicate := seen[instance.ID]; duplicate {
			return Session{}, Fail(ErrInvalidInput, "orthanc returned a duplicate instance identifier")
		}
		seen[instance.ID] = struct{}{}
		uploaded = append(uploaded, instance)
	}

	groups := groupBySeries(uploaded)
	if len(groups) > s.limits.MaxSeries {
		return Session{}, Fail(ErrInvalidInput, "upload spans too many series")
	}

	series := make([]seriesState, 0, len(groups))
	for index, group := range groups {
		if ctx.Err() != nil {
			return Session{}, Fail(ErrCanceled, "upload canceled during inspection")
		}
		metadata, err := s.archive.DescribeSeries(ctx, group.seriesID)
		if err != nil {
			return Session{}, err
		}
		option := SeriesOption{
			Index:         index + 1,
			InstanceCount: len(group.instances),
			Modality:      seriesModality(metadata),
		}
		if metadata.SeriesID != group.seriesID || metadata.StudyID != group.studyID {
			option.ReasonCode = ReasonUnsupported
		} else if _, err := ValidateSeries(metadata, group.instances); err != nil {
			option.ReasonCode = unsuitableReason(metadata, group.instances)
		} else {
			option.Suitable = true
		}
		key, err := randomToken()
		if err != nil {
			return Session{}, Fail(ErrDependency, "cannot allocate a series key")
		}
		option.ID = key
		series = append(series, seriesState{
			key:             key,
			option:          option,
			orthancSeriesID: group.seriesID,
			studyID:         group.studyID,
			instances:       group.instances,
		})
	}

	token, err := randomToken()
	if err != nil {
		return Session{}, Fail(ErrDependency, "cannot allocate an upload session")
	}
	now := time.Now()
	state := &sessionState{id: token, expiresAt: now.Add(s.limits.SessionTTL), skipped: skippedFiles, series: series}
	s.mu.Lock()
	s.evictExpiredLocked(now)
	if len(s.sessions) >= s.limits.MaxSessions {
		s.mu.Unlock()
		return Session{}, Fail(ErrCapacity, "too many upload sessions are open")
	}
	s.sessions[token] = state
	s.mu.Unlock()
	return state.view(), nil
}

// Confirm re-verifies one prepared series and creates exactly one case. A
// repeated confirm of the same series returns the same result; a different
// series after a successful confirm is refused. An uncertain commit leaves the
// session unconfirmed so Get can reconcile it later.
func (s *Service) Confirm(ctx context.Context, uploadID, seriesKey string) (Result, error) {
	if ctx.Err() != nil {
		return Result{}, Fail(ErrCanceled, "upload canceled before confirmation")
	}
	state, err := s.session(uploadID)
	if err != nil {
		return Result{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if expired(state, time.Now()) {
		return Result{}, Fail(ErrExpired, "upload session expired")
	}
	if state.confirmed {
		if seriesKey == state.selectedKey {
			return state.result(), nil
		}
		return Result{}, Fail(ErrAlreadyConfirmed, "upload session already confirmed")
	}
	group, ok := state.lookupSeries(seriesKey)
	if !ok {
		return Result{}, Fail(ErrInvalidInput, "unknown series selection")
	}

	metadata, err := s.archive.DescribeSeries(ctx, group.orthancSeriesID)
	if err != nil {
		return Result{}, err
	}
	if metadata.SeriesID != group.orthancSeriesID || metadata.StudyID != group.studyID {
		return Result{}, Fail(ErrUnsuitableSeries, "series metadata no longer matches the uploaded instances")
	}
	imported, err := ValidateSeries(metadata, group.instances)
	if err != nil {
		return Result{}, err
	}
	inventory, err := s.archive.ListSeriesInstances(ctx, group.orthancSeriesID)
	if err != nil {
		return Result{}, err
	}
	if !sameInstanceSet(inventory, instanceIDs(group.instances)) {
		return Result{}, Fail(ErrDependency, "series inventory changed during confirmation")
	}
	imported.Source = s.source

	existingID, found, err := s.store.FindCaseBySeriesUID(ctx, imported.SeriesInstanceUID)
	if err != nil {
		return Result{}, err
	}
	if found {
		state.markConfirmed(seriesKey, existingID, true, imported.InstanceCount)
		return state.result(), nil
	}

	caseID, err := s.store.CreateImportedCase(ctx, imported)
	if err != nil {
		if errors.Is(err, ErrDuplicateSeries) {
			duplicateID, duplicateFound, lookupErr := s.store.FindCaseBySeriesUID(ctx, imported.SeriesInstanceUID)
			if lookupErr != nil {
				return Result{}, lookupErr
			}
			if duplicateFound {
				state.markConfirmed(seriesKey, duplicateID, true, imported.InstanceCount)
				return state.result(), nil
			}
		}
		if errors.Is(err, ErrCommitOutcomeUnknown) {
			state.attemptKey = seriesKey
			state.attemptUID = imported.SeriesInstanceUID
			state.attemptCount = imported.InstanceCount
		}
		return Result{}, err
	}
	state.markConfirmed(seriesKey, caseID, false, imported.InstanceCount)
	return state.result(), nil
}

// Get returns one session's redacted view. When a previous confirm ended with
// an unknown commit outcome it re-checks the case by the validated Series
// Instance UID and completes the session only when the row is really visible.
// An unreachable database leaves the session unconfirmed and returns no error.
func (s *Service) Get(ctx context.Context, uploadID string) (Session, error) {
	state, err := s.session(uploadID)
	if err != nil {
		return Session{}, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if expired(state, time.Now()) {
		return Session{}, Fail(ErrExpired, "upload session expired")
	}
	if !state.confirmed && state.attemptUID != "" {
		caseID, found, lookupErr := s.store.FindCaseBySeriesUID(ctx, state.attemptUID)
		if lookupErr == nil && found {
			state.markConfirmed(state.attemptKey, caseID, true, state.attemptCount)
		}
	}
	return state.view(), nil
}

// Discard drops an unconfirmed session. It never deletes a case or any Orthanc
// data; an already expired session is reported as not found.
func (s *Service) Discard(ctx context.Context, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sessions[uploadID]
	if !ok {
		return Fail(ErrNotFound, "unknown upload session")
	}
	if expired(state, time.Now()) {
		delete(s.sessions, uploadID)
		return Fail(ErrExpired, "upload session expired")
	}
	delete(s.sessions, uploadID)
	return nil
}

// session looks up a live session, evicting an expired one.
func (s *Service) session(uploadID string) (*sessionState, error) {
	if uploadID == "" {
		return nil, Fail(ErrNotFound, "unknown upload session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sessions[uploadID]
	if !ok {
		return nil, Fail(ErrNotFound, "unknown upload session")
	}
	if expired(state, time.Now()) {
		delete(s.sessions, uploadID)
		return nil, Fail(ErrExpired, "upload session expired")
	}
	return state, nil
}

// evictExpiredLocked drops every expired session while s.mu is held.
func (s *Service) evictExpiredLocked(now time.Time) {
	for id, state := range s.sessions {
		if expired(state, now) {
			delete(s.sessions, id)
		}
	}
}

// expired reports whether a session is past its lifetime.
func expired(state *sessionState, now time.Time) bool {
	return !state.expiresAt.After(now)
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
