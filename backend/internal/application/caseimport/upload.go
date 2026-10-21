package caseimport

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Default session budgets. They bound in-memory upload sessions: how long a
// prepared session stays selectable, how many may be open at once and how many
// series one upload may span.
const (
	defaultSessionTTL  = 30 * time.Minute
	defaultMaxSessions = 8
	defaultMaxSeries   = 64
)

// uploadSource is the fixed, non-identifying case source every upload sets. It
// never comes from the client.
const uploadSource = "upload"

// Fixed reason codes for a series the upload cannot offer. They are the only
// suitability detail the API discloses before the initial read is locked.
const (
	// ReasonSingleSlice reports fewer than two slices.
	ReasonSingleSlice = "SINGLE_SLICE"
	// ReasonMultiFrame reports an enhanced/multiframe CT series.
	ReasonMultiFrame = "MULTIFRAME"
	// ReasonUnsupported reports any other unsupported geometry.
	ReasonUnsupported = "UNSUPPORTED_SERIES"
)

// StagedFile is one file the upload intake already wrote into a private
// workspace. The path is server-generated; the size was measured on disk.
type StagedFile struct {
	Path string
	Size int64
}

// SeriesOption is the redacted, per-session view of one uploaded series. ID is
// an opaque session key, never an Orthanc identifier; no UID, path or patient
// tag is exposed.
type SeriesOption struct {
	ID            string
	Index         int
	Modality      string
	InstanceCount int
	Suitable      bool
	ReasonCode    string
}

// Session is the redacted wire view of one prepared upload.
type Session struct {
	ID               string
	ExpiresAt        time.Time
	SkippedFiles     int
	Series           []SeriesOption
	SelectedSeriesID string
	CaseID           int64
}

// Limits bound one upload session. Zero values fall back to the defaults.
type Limits struct {
	SessionTTL  time.Duration
	MaxSessions int
	MaxSeries   int
}

// DefaultLimits returns the documented session budgets.
func DefaultLimits() Limits {
	return Limits{SessionTTL: defaultSessionTTL, MaxSessions: defaultMaxSessions, MaxSeries: defaultMaxSeries}
}

// seriesState is the private half of a session: the Orthanc linkage and the
// uploaded instance set used to re-verify a series before it is committed.
type seriesState struct {
	key             string
	option          SeriesOption
	orthancSeriesID string
	studyID         string
	instances       []UploadedInstance
}

// sessionState is one prepared upload. Its mutex serializes confirmation so two
// requests for the same session can never create two cases.
type sessionState struct {
	mu            sync.Mutex
	id            string
	ownerUserID   int64
	expiresAt     time.Time
	skipped       int
	series        []seriesState
	confirmed     bool
	selectedKey   string
	caseID        int64
	existing      bool
	instanceCount int
	// attemptKey/attemptUID record an uncertain confirm so a later read can
	// reconcile it without replaying the write.
	attemptKey   string
	attemptUID   string
	attemptCount int
}

func (st *sessionState) lookupSeries(key string) (seriesState, bool) {
	for _, series := range st.series {
		if series.key == key {
			return series, true
		}
	}
	return seriesState{}, false
}

func (st *sessionState) markConfirmed(key string, caseID int64, existing bool, instanceCount int) {
	st.confirmed = true
	st.selectedKey = key
	st.caseID = caseID
	st.existing = existing
	st.instanceCount = instanceCount
}

func (st *sessionState) result() Result {
	return Result{CaseID: st.caseID, InstanceCount: st.instanceCount, Existing: st.existing}
}

// view renders the redacted session, always with a fresh series slice so a
// caller cannot mutate the stored state.
func (st *sessionState) view() Session {
	options := make([]SeriesOption, 0, len(st.series))
	for _, series := range st.series {
		options = append(options, series.option)
	}
	selected := ""
	if st.confirmed {
		selected = st.selectedKey
	}
	return Session{
		ID:               st.id,
		ExpiresAt:        st.expiresAt,
		SkippedFiles:     st.skipped,
		Series:           options,
		SelectedSeriesID: selected,
		CaseID:           st.caseID,
	}
}

// seriesGroup collects the accepted instances of one study/series.
type seriesGroup struct {
	seriesID  string
	studyID   string
	instances []UploadedInstance
}

// groupBySeries buckets accepted uploads by their parent, preserving the order
// in which each series first appears.
func groupBySeries(uploaded []UploadedInstance) []seriesGroup {
	groups := make([]seriesGroup, 0, 4)
	index := make(map[string]int, 4)
	for _, instance := range uploaded {
		key := instance.StudyID + "\x00" + instance.SeriesID
		position, ok := index[key]
		if !ok {
			position = len(groups)
			index[key] = position
			groups = append(groups, seriesGroup{seriesID: instance.SeriesID, studyID: instance.StudyID})
		}
		groups[position].instances = append(groups[position].instances, instance)
	}
	return groups
}

// unsuitableReason classifies why a described series cannot be offered. The
// distinctions are the only suitability detail the API exposes.
func unsuitableReason(metadata SeriesMetadata, uploaded []UploadedInstance) string {
	if len(uploaded) < 2 || len(metadata.Instances) < 2 {
		return ReasonSingleSlice
	}
	for _, instance := range metadata.Instances {
		if instance.NumberOfFrames > 1 {
			return ReasonMultiFrame
		}
	}
	return ReasonUnsupported
}

// seriesModality returns the sanitized modality of a described series, or the
// empty string when it cannot be rendered safely.
func seriesModality(metadata SeriesMetadata) string {
	if len(metadata.Instances) == 0 {
		return ""
	}
	raw := strings.Trim(metadata.Instances[0].Modality, "\x00 ")
	out := make([]rune, 0, 8)
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out = append(out, unicode.ToUpper(r))
		}
		if len(out) == 8 {
			break
		}
	}
	return string(out)
}

// instanceIDs builds the accepted-instance set of one series.
func instanceIDs(instances []UploadedInstance) map[string]struct{} {
	ids := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		ids[instance.ID] = struct{}{}
	}
	return ids
}

// randomToken returns an opaque session identifier with at least 128 bits of
// entropy from crypto/rand.
func randomToken() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
