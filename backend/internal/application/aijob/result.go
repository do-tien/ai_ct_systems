// Package aijob implements the LC1-12 AI job use case: a startup drain that
// claims queued jobs, runs the pinned inference CLI on a retrieved DICOM series
// and persists only a fully validated result or a sanitized failure. It depends
// only on the standard library and the ports declared here.
package aijob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Job is one claimed job with its immutable case linkage.
type Job struct {
	ID                  int64
	CaseID              int64
	OrthancSeriesID     string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
}

// Failure is a sanitized terminal job failure: a fixed code from this package
// or the inference CLI's safe code set, plus the redacted private stderr tail.
type Failure struct {
	Code       string
	StderrTail string
}

// Archive is the consumer-owned Orthanc port: it streams one series archive
// into dst. The implementation validates the series identifier and never
// follows redirects.
type Archive interface {
	Download(ctx context.Context, seriesID string, dst io.Writer) error
}

// Execution is one completed inference process outcome. Result is nil when no
// result file exists; it is validated by the use case, never trusted here.
type Execution struct {
	ExitCode   int
	Result     json.RawMessage
	StderrTail string
}

// CodeError is a runner failure carrying a fixed infrastructure code and the
// already-sanitized stderr tail. Its message is the code only, never an
// upstream error string.
type CodeError struct {
	Code       string
	StderrTail string
}

// Error returns the fixed infrastructure code.
func (e *CodeError) Error() string { return e.Code }

// MaxStderrTailBytes bounds the stored private stderr diagnostic. It matches
// the 4 KiB POC ceiling.
const MaxStderrTailBytes = 4096

// Infrastructure failure codes recorded in ai_jobs.error_code. They describe
// orchestration outcomes, never a patient- or model-derived value.
const (
	CodeOrthancArchiveError = "ORTHANC_ARCHIVE_ERROR"
	CodeArchiveInvalid      = "ARCHIVE_INVALID"
	CodeWorkspaceError      = "WORKSPACE_ERROR"
	CodeProcessStartFailed  = "PROCESS_START_FAILED"
	CodeAITimeout           = "AI_TIMEOUT"
	CodeAICanceled          = "AI_CANCELED"
	CodeResultMissing       = "RESULT_MISSING"
	CodeResultInvalid       = "RESULT_INVALID"
	CodeProcessError        = "PROCESS_ERROR"
)

// Sentinel categories shared between the store adapter and the use case.
var (
	// ErrOutcomeUnknown marks a claim or terminal write whose outcome the
	// adapter cannot determine (for example a lost connection mid-commit). The
	// worker must not launch Python or retry on this category.
	ErrOutcomeUnknown = errors.New("database operation outcome is unknown")
	// ErrConflict marks a rejected job transition (a conditional update that
	// matched no row).
	ErrConflict = errors.New("job transition rejected")
	// ErrDependency marks a definite database failure.
	ErrDependency = errors.New("database operation failed")
)

// expectedBundle and expectedBundleVersion pin the only accepted detector
// revision. These checks prove the document's self-declared metadata, not real
// model provenance or coordinate placement.
const (
	expectedBundle        = "lung_nodule_ct_detection"
	expectedBundleVersion = "0.6.11"
	expectedThreshold     = 0.5
	// timingToleranceS matches Python's TIMING_TOLERANCE_S = 1e-6.
	timingToleranceS = 1e-6
	// maxUIDLength matches the DICOM UID syntax limit used by the CLI.
	maxUIDLength = 64
)

var uidPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`)

// cliSafeCodes is the allowlisted failure-code set of the current inference
// CLI (service_exampl/src/nodule_ai/infer.py:SAFE_MESSAGES). A code outside
// this set is treated as an invalid result, never persisted verbatim.
var cliSafeCodes = map[string]struct{}{
	"INVALID_ARGUMENT":            {},
	"INVALID_OUTPUT_PATH":         {},
	"TOOL_ERROR":                  {},
	"IO_ERROR":                    {},
	"RESOURCE_MEASUREMENT_FAILED": {},
	"INTERNAL_ERROR":              {},
	"DEVICE_UNAVAILABLE":          {},
	"BUNDLE_INVALID":              {},
	"MODEL_LOAD_FAILED":           {},
	"MODEL_OOM":                   {},
	"INFERENCE_FAILED":            {},
	"INVALID_MODEL_OUTPUT":        {},
	"INVALID_DICOM":               {},
	"NOT_CT":                      {},
	"UNSUPPORTED_CT":              {},
	"INVALID_UID":                 {},
	"UID_MISMATCH":                {},
	"DUPLICATE_SOP":               {},
	"INSUFFICIENT_SLICES":         {},
	"INVALID_GEOMETRY":            {},
	"DUPLICATE_POSITION":          {},
	"NONUNIFORM_SPACING":          {},
	"SLICE_TOO_THICK":             {},
	"SLICE_SPACING_TOO_LARGE":     {},
	"UNSUPPORTED_GANTRY_TILT":     {},
	"INCOMPLETE_SERIES":           {},
	"PIXEL_DECODE_FAILED":         {},
	"INVALID_PIXEL_DATA":          {},
	"GEOMETRY_MISMATCH":           {},
	"NIFTI_AFFINE_MISMATCH":       {},
	"NIFTI_WRITE_FAILED":          {},
	"LANDMARK_OUT_OF_BOUNDS":      {},
	"INVALID_REFERENCE":           {},
}

func isCLISafeCode(code string) bool {
	_, ok := cliSafeCodes[code]
	return ok
}

// IsCLISafeCode reports whether a code belongs to the current CLI's fixed
// SAFE_MESSAGES set. A code outside it must never be persisted verbatim.
func IsCLISafeCode(code string) bool {
	return isCLISafeCode(code)
}

// SanitizeStderrTail reduces a captured stderr tail to at most
// MaxStderrTailBytes: it keeps only complete lines exactly matching
// "infer: <allowlisted CLI code>" and replaces every other line, including a
// partial first line cut off by tailing, with "[redacted]". It is not a regex
// privacy guarantee; it is a fixed allowlist of lines.
func SanitizeStderrTail(tail string) string {
	if tail == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(tail, "\n"), "\n")
	sanitized := make([]string, 0, len(lines))
	for _, line := range lines {
		if code, ok := cliLineCode(line); ok {
			sanitized = append(sanitized, "infer: "+code)
		} else {
			sanitized = append(sanitized, "[redacted]")
		}
	}
	out := strings.Join(sanitized, "\n")
	if len(out) > MaxStderrTailBytes {
		out = out[:MaxStderrTailBytes]
		for len(out) > 0 && !utf8.ValidString(out) {
			out = out[:len(out)-1]
		}
	}
	return out
}

// cliLineCode returns the allowlisted code of an exact "infer: <code>" line.
func cliLineCode(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	const prefix = "infer: "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	code := line[len(prefix):]
	if code == "" || strings.ContainsAny(code, " \t") {
		return "", false
	}
	if !isCLISafeCode(code) {
		return "", false
	}
	return code, true
}

// ValidateSuccess validates a success document against the job linkage and the
// settled LC1-01 §6 contract. It returns nil only when the whole document is
// consistent; the caller persists the original raw bytes for JSONB.
func ValidateSuccess(raw json.RawMessage, job Job) error {
	obj, err := decodeStrictObject(raw)
	if err != nil {
		return fmt.Errorf("result is not strict JSON: %w", err)
	}

	version, err := stringField(obj, "schema_version")
	if err != nil {
		return err
	}
	if version != "1" {
		return errors.New("schema_version must be the string \"1\"")
	}
	if _, present := obj["error"]; present {
		return errors.New("a success document must not carry an error payload")
	}

	if _, err := matchingUID(obj, "series_instance_uid", job.SeriesInstanceUID); err != nil {
		return err
	}
	if _, err := matchingUID(obj, "frame_of_reference_uid", job.FrameOfReferenceUID); err != nil {
		return err
	}

	coordinate, err := stringField(obj, "coordinate_system")
	if err != nil {
		return err
	}
	if coordinate != "DICOM_LPS_mm" {
		return errors.New("coordinate_system must be DICOM_LPS_mm")
	}

	if err := validateModel(obj); err != nil {
		return err
	}
	if err := validateInput(obj); err != nil {
		return err
	}
	if err := validateCandidates(obj); err != nil {
		return err
	}
	if err := validateTiming(obj); err != nil {
		return err
	}
	if err := validateResources(obj); err != nil {
		return err
	}
	return nil
}

// ValidateFailure validates the version-1 error envelope and returns the
// allowlisted CLI code. The message is never returned or persisted; any
// malformed or non-allowlisted envelope is an error the caller maps to
// RESULT_INVALID.
func ValidateFailure(raw json.RawMessage) (string, error) {
	obj, err := decodeStrictObject(raw)
	if err != nil {
		return "", fmt.Errorf("error document is not strict JSON: %w", err)
	}
	if len(obj) != 2 {
		return "", errors.New("error envelope must have exactly schema_version and error")
	}
	version, err := stringField(obj, "schema_version")
	if err != nil {
		return "", err
	}
	if version != "1" {
		return "", errors.New("schema_version must be the string \"1\"")
	}
	envelope, ok := obj["error"].(map[string]any)
	if !ok {
		return "", errors.New("error must be an object")
	}
	if len(envelope) != 2 {
		return "", errors.New("error object must have exactly code and message")
	}
	code, err := stringField(envelope, "code")
	if err != nil {
		return "", err
	}
	if _, err := stringField(envelope, "message"); err != nil {
		return "", err
	}
	if !isCLISafeCode(code) {
		return "", errors.New("error code is not in the CLI safe code set")
	}
	return code, nil
}

func validateModel(obj map[string]any) error {
	model, err := objectField(obj, "model")
	if err != nil {
		return err
	}
	bundle, err := stringField(model, "bundle")
	if err != nil {
		return err
	}
	if bundle != expectedBundle {
		return errors.New("model.bundle is not the expected detector bundle")
	}
	bundleVersion, err := stringField(model, "bundle_version")
	if err != nil {
		return err
	}
	if bundleVersion != expectedBundleVersion {
		return errors.New("model.bundle_version is not the expected revision")
	}
	if _, err := stringField(model, "monai_version"); err != nil {
		return err
	}
	if _, err := stringField(model, "torch_version"); err != nil {
		return err
	}
	affine, err := boolField(model, "affine_lps_to_ras")
	if err != nil {
		return err
	}
	if !affine {
		return errors.New("model.affine_lps_to_ras must be true")
	}
	threshold, err := numberField(model, "score_threshold")
	if err != nil {
		return err
	}
	if threshold != expectedThreshold {
		return errors.New("model.score_threshold must be 0.5")
	}
	return nil
}

func validateInput(obj map[string]any) error {
	input, err := objectField(obj, "input")
	if err != nil {
		return err
	}
	slices, err := integerField(input, "num_slices")
	if err != nil {
		return err
	}
	if slices <= 0 {
		return errors.New("input.num_slices must be positive")
	}
	if _, err := tripleField(input, "spacing_mm", true); err != nil {
		return err
	}
	if _, err := tripleField(input, "size_voxels", true); err != nil {
		return err
	}
	return nil
}

func validateCandidates(obj map[string]any) error {
	candidates, err := listField(obj, "candidates")
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(candidates))
	for index, entry := range candidates {
		candidate, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("candidates[%d] must be an object", index)
		}
		id, err := stringField(candidate, "id")
		if err != nil {
			return fmt.Errorf("candidates[%d]: %w", index, err)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("candidates[%d].id is not unique", index)
		}
		seen[id] = struct{}{}
		if _, err := tripleField(candidate, "center_mm", false); err != nil {
			return fmt.Errorf("candidates[%d]: %w", index, err)
		}
		if _, err := tripleField(candidate, "size_mm", true); err != nil {
			return fmt.Errorf("candidates[%d]: %w", index, err)
		}
		score, err := numberField(candidate, "score")
		if err != nil {
			return fmt.Errorf("candidates[%d]: %w", index, err)
		}
		if score < 0 || score > 1 {
			return fmt.Errorf("candidates[%d].score must be within [0, 1]", index)
		}
	}
	return nil
}

func validateTiming(obj map[string]any) error {
	timing, err := objectField(obj, "timing_s")
	if err != nil {
		return err
	}
	load, err := numberField(timing, "load")
	if err != nil {
		return err
	}
	inference, err := numberField(timing, "inference")
	if err != nil {
		return err
	}
	total, err := numberField(timing, "total")
	if err != nil {
		return err
	}
	if load < 0 || inference < 0 || total < 0 {
		return errors.New("timing_s values must be non-negative")
	}
	if total+timingToleranceS < load+inference {
		return errors.New("timing_s.total must cover load plus inference")
	}
	return nil
}

func validateResources(obj map[string]any) error {
	resources, err := objectField(obj, "resources")
	if err != nil {
		return err
	}
	if _, err := stringField(resources, "device"); err != nil {
		return err
	}
	for _, name := range []string{"peak_gpu_mem_mb", "peak_rss_mb"} {
		value, err := numberField(resources, name)
		if err != nil {
			return err
		}
		if value < 0 {
			return fmt.Errorf("resources.%s must be non-negative", name)
		}
	}
	return nil
}

// --- presence-aware typed accessors over the strictly decoded tree ---

func matchingUID(obj map[string]any, name string, expected string) (string, error) {
	value, err := stringField(obj, name)
	if err != nil {
		return "", err
	}
	if len(value) > maxUIDLength || !uidPattern.MatchString(value) {
		return "", fmt.Errorf("%s is not a valid DICOM UID", name)
	}
	if value != expected {
		return "", fmt.Errorf("%s does not match the claimed case", name)
	}
	return value, nil
}

func objectField(obj map[string]any, name string) (map[string]any, error) {
	value, present := obj[name]
	if !present {
		return nil, fmt.Errorf("%s is required", name)
	}
	nested, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return nested, nil
}

func listField(obj map[string]any, name string) ([]any, error) {
	value, present := obj[name]
	if !present {
		return nil, fmt.Errorf("%s is required", name)
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list", name)
	}
	return list, nil
}

func stringField(obj map[string]any, name string) (string, error) {
	value, present := obj[name]
	if !present {
		return "", fmt.Errorf("%s is required", name)
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return text, nil
}

func boolField(obj map[string]any, name string) (bool, error) {
	value, present := obj[name]
	if !present {
		return false, fmt.Errorf("%s is required", name)
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return flag, nil
}

func numberField(obj map[string]any, name string) (float64, error) {
	value, present := obj[name]
	if !present {
		return 0, fmt.Errorf("%s is required", name)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be a number", name)
	}
	parsed, err := number.Float64()
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s must be a finite number", name)
	}
	return parsed, nil
}

func integerField(obj map[string]any, name string) (int64, error) {
	value, present := obj[name]
	if !present {
		return 0, fmt.Errorf("%s is required", name)
	}
	if _, isBool := value.(bool); isBool {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func tripleField(obj map[string]any, name string, positive bool) ([3]float64, error) {
	var out [3]float64
	list, err := listField(obj, name)
	if err != nil {
		return out, err
	}
	if len(list) != 3 {
		return out, fmt.Errorf("%s must be three numbers", name)
	}
	for index, entry := range list {
		number, ok := entry.(json.Number)
		if !ok {
			return out, fmt.Errorf("%s[%d] must be a number", name, index)
		}
		parsed, err := number.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return out, fmt.Errorf("%s[%d] must be a finite number", name, index)
		}
		if positive && parsed <= 0 {
			return out, fmt.Errorf("%s must be positive", name)
		}
		out[index] = parsed
	}
	return out, nil
}

// --- strict JSON decoding ---

var (
	errNotObject     = errors.New("top level must be a JSON object")
	errTrailingInput = errors.New("unexpected trailing JSON content")
	errDuplicateKey  = errors.New("duplicate object key")
)

// decodeStrictObject decodes exactly one JSON object, rejecting duplicate keys
// at any depth and rejecting numbers that overflow to a non-finite value. It
// keeps the original bytes untouched for JSONB persistence.
func decodeStrictObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errTrailingInput
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errNotObject
	}
	return object, nil
}

func decodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			return decodeObject(decoder)
		case '[':
			return decodeArray(decoder)
		}
		return nil, fmt.Errorf("unexpected delimiter %q", value)
	case json.Number:
		parsed, err := value.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, errors.New("number is not finite")
		}
		return value, nil
	default:
		return token, nil
	}
}

func decodeObject(decoder *json.Decoder) (map[string]any, error) {
	object := make(map[string]any)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		if _, duplicate := object[key]; duplicate {
			return nil, errDuplicateKey
		}
		value, err := decodeValue(decoder)
		if err != nil {
			return nil, err
		}
		object[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeArray(decoder *json.Decoder) ([]any, error) {
	array := []any{}
	for decoder.More() {
		value, err := decodeValue(decoder)
		if err != nil {
			return nil, err
		}
		array = append(array, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	return array, nil
}
