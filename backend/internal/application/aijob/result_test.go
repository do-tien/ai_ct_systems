package aijob

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	testSeriesUID  = "1.2.840.113619.2.55.3.604688427.1.1"
	testFrameUID   = "1.2.840.113619.2.55.3.604688427.2.1"
	testOtherUID   = "1.2.840.113619.2.55.3.604688427.9.9"
	testSecretPath = "/home/secret/bundle/is/broken"
)

func testJob() Job {
	return Job{
		ID:                  7,
		CaseID:              3,
		OrthancSeriesID:     "aaaaaaaa-bbbbbbbb-cccccccc-dddddddd-eeeeeeee",
		SeriesInstanceUID:   testSeriesUID,
		FrameOfReferenceUID: testFrameUID,
	}
}

// validSuccessDocument returns a fresh, explicitly synthetic success document.
// Callers may mutate it before marshalling.
func validSuccessDocument() map[string]any {
	return map[string]any{
		"schema_version":         "1",
		"series_instance_uid":    testSeriesUID,
		"frame_of_reference_uid": testFrameUID,
		"coordinate_system":      "DICOM_LPS_mm",
		"model": map[string]any{
			"bundle":            "lung_nodule_ct_detection",
			"bundle_version":    "0.6.11",
			"monai_version":     "1.4.0",
			"torch_version":     "2.5.1",
			"affine_lps_to_ras": true,
			"score_threshold":   0.5,
		},
		"input": map[string]any{
			"num_slices":  133,
			"spacing_mm":  []any{0.7, 0.7, 1.25},
			"size_voxels": []any{512, 512, 133},
		},
		"candidates": []any{
			map[string]any{
				"id":        "c1",
				"center_mm": []any{-96.25, 39.05, -180.5},
				"size_mm":   []any{17.0, 17.0, 17.0},
				"score":     0.94,
			},
		},
		"timing_s":  map[string]any{"load": 1.5, "inference": 12.0, "total": 13.5},
		"resources": map[string]any{"device": "cuda", "peak_gpu_mem_mb": 4096.0, "peak_rss_mb": 2048.0},
	}
}

func mustJSON(t *testing.T, doc any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}

func TestValidateSuccessAcceptsSyntheticComplete(t *testing.T) {
	raw := mustJSON(t, validSuccessDocument())
	if err := ValidateSuccess(raw, testJob()); err != nil {
		t.Fatalf("ValidateSuccess() error = %v, want nil", err)
	}
}

func TestValidateSuccessAcceptsEmptyCandidates(t *testing.T) {
	doc := validSuccessDocument()
	doc["candidates"] = []any{}
	if err := ValidateSuccess(mustJSON(t, doc), testJob()); err != nil {
		t.Fatalf("ValidateSuccess(empty candidates) error = %v, want nil", err)
	}
}

func TestValidateSuccessAcceptsScoreBoundsAndNegativeCenters(t *testing.T) {
	doc := validSuccessDocument()
	doc["candidates"] = []any{
		map[string]any{"id": "c1", "center_mm": []any{-1.0, -2.0, -3.0}, "size_mm": []any{3.0, 3.0, 3.0}, "score": 0.0},
		map[string]any{"id": "c2", "center_mm": []any{1.0, 2.0, 3.0}, "size_mm": []any{3.0, 3.0, 3.0}, "score": 1.0},
	}
	if err := ValidateSuccess(mustJSON(t, doc), testJob()); err != nil {
		t.Fatalf("ValidateSuccess(boundary scores) error = %v, want nil", err)
	}
}

func TestValidateSuccessRetainsUnknownAdditiveMetadata(t *testing.T) {
	doc := validSuccessDocument()
	doc["future_field"] = map[string]any{"note": "additive", "n": 3.0}
	if err := ValidateSuccess(mustJSON(t, doc), testJob()); err != nil {
		t.Fatalf("ValidateSuccess(additive metadata) error = %v, want nil", err)
	}
}

// mutate returns a copy of the base document with fn applied, then validates.
func validateMutated(t *testing.T, fn func(doc map[string]any)) error {
	t.Helper()
	doc := validSuccessDocument()
	fn(doc)
	return ValidateSuccess(mustJSON(t, doc), testJob())
}

func TestValidateSuccessRejectsMalformedDocument(t *testing.T) {
	cases := map[string]func(doc map[string]any){
		"wrong schema version":      func(d map[string]any) { d["schema_version"] = "2" },
		"schema version not string": func(d map[string]any) { d["schema_version"] = 1.0 },
		"coordinate system":         func(d map[string]any) { d["coordinate_system"] = "RAS_mm" },
		"series uid mismatch":       func(d map[string]any) { d["series_instance_uid"] = testOtherUID },
		"frame uid mismatch":        func(d map[string]any) { d["frame_of_reference_uid"] = testOtherUID },
		"series uid not syntax":     func(d map[string]any) { d["series_instance_uid"] = "not.a.uid" },
		"missing coordinate system": func(d map[string]any) { delete(d, "coordinate_system") },
		"null series uid":           func(d map[string]any) { d["series_instance_uid"] = nil },
		"missing candidates":        func(d map[string]any) { delete(d, "candidates") },
		"null candidates":           func(d map[string]any) { d["candidates"] = nil },
		"mixed error payload":       func(d map[string]any) { d["error"] = map[string]any{"code": "X", "message": "y"} },
		"model null":                func(d map[string]any) { d["model"] = nil },
		"model missing key":         func(d map[string]any) { delete(d["model"].(map[string]any), "torch_version") },
		"empty monai version":       func(d map[string]any) { d["model"].(map[string]any)["monai_version"] = "" },
		"affine flag not bool":      func(d map[string]any) { d["model"].(map[string]any)["affine_lps_to_ras"] = "true" },
		"affine flag false":         func(d map[string]any) { d["model"].(map[string]any)["affine_lps_to_ras"] = false },
		"bundle wrong":              func(d map[string]any) { d["model"].(map[string]any)["bundle"] = "other_bundle" },
		"bundle version wrong":      func(d map[string]any) { d["model"].(map[string]any)["bundle_version"] = "0.6.10" },
		"threshold not 0.5":         func(d map[string]any) { d["model"].(map[string]any)["score_threshold"] = 0.6 },
		"threshold bool":            func(d map[string]any) { d["model"].(map[string]any)["score_threshold"] = true },
		"num slices zero":           func(d map[string]any) { d["input"].(map[string]any)["num_slices"] = 0 },
		"num slices bool":           func(d map[string]any) { d["input"].(map[string]any)["num_slices"] = true },
		"num slices fractional":     func(d map[string]any) { d["input"].(map[string]any)["num_slices"] = 1.5 },
		"spacing nonpositive":       func(d map[string]any) { d["input"].(map[string]any)["spacing_mm"] = []any{0.7, 0.0, 1.25} },
		"spacing wrong length":      func(d map[string]any) { d["input"].(map[string]any)["spacing_mm"] = []any{0.7, 0.7} },
		"size voxels nonpositive":   func(d map[string]any) { d["input"].(map[string]any)["size_voxels"] = []any{512, 512, 0} },
		"size voxels string":        func(d map[string]any) { d["input"].(map[string]any)["size_voxels"] = []any{"512", 512, 133} },
		"input missing":             func(d map[string]any) { delete(d, "input") },
		"candidate duplicate id": func(d map[string]any) {
			d["candidates"] = []any{
				map[string]any{"id": "c1", "center_mm": []any{0.0, 0.0, 0.0}, "size_mm": []any{3.0, 3.0, 3.0}, "score": 0.9},
				map[string]any{"id": "c1", "center_mm": []any{1.0, 1.0, 1.0}, "size_mm": []any{3.0, 3.0, 3.0}, "score": 0.8},
			}
		},
		"candidate blank id":      func(d map[string]any) { d["candidates"].([]any)[0].(map[string]any)["id"] = "" },
		"candidate id not string": func(d map[string]any) { d["candidates"].([]any)[0].(map[string]any)["id"] = 1.0 },
		"candidate size nonpositive": func(d map[string]any) {
			d["candidates"].([]any)[0].(map[string]any)["size_mm"] = []any{17.0, 0.0, 17.0}
		},
		"candidate center wrong length": func(d map[string]any) {
			d["candidates"].([]any)[0].(map[string]any)["center_mm"] = []any{0.0, 0.0}
		},
		"candidate score below zero": func(d map[string]any) { d["candidates"].([]any)[0].(map[string]any)["score"] = -0.1 },
		"candidate score above one":  func(d map[string]any) { d["candidates"].([]any)[0].(map[string]any)["score"] = 1.1 },
		"candidate score string":     func(d map[string]any) { d["candidates"].([]any)[0].(map[string]any)["score"] = "0.9" },
		"candidate not object":       func(d map[string]any) { d["candidates"] = []any{3.0} },
		"timing total below sum":     func(d map[string]any) { d["timing_s"].(map[string]any)["total"] = 1.0 },
		"timing negative":            func(d map[string]any) { d["timing_s"].(map[string]any)["load"] = -1.0 },
		"timing missing":             func(d map[string]any) { delete(d, "timing_s") },
		"resources memory negative":  func(d map[string]any) { d["resources"].(map[string]any)["peak_rss_mb"] = -1.0 },
		"resources device empty":     func(d map[string]any) { d["resources"].(map[string]any)["device"] = "" },
		"resources missing":          func(d map[string]any) { delete(d, "resources") },
		"top level not object":       func(d map[string]any) { delete(d, "schema_version") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateMutated(t, mutate); err == nil {
				t.Fatalf("ValidateSuccess() accepted a malformed document")
			}
		})
	}
}

func TestValidateSuccessRejectsNonObjectTopLevel(t *testing.T) {
	if err := ValidateSuccess(json.RawMessage(`[1,2,3]`), testJob()); err == nil {
		t.Fatal("ValidateSuccess(list) accepted a non-object document")
	}
	if err := ValidateSuccess(json.RawMessage(`"text"`), testJob()); err == nil {
		t.Fatal("ValidateSuccess(string) accepted a non-object document")
	}
}

func TestValidateSuccessRejectsMalformedUIDAgainstJob(t *testing.T) {
	doc := validSuccessDocument()
	badJob := testJob()
	badJob.SeriesInstanceUID = "1..2"
	doc["series_instance_uid"] = "1..2"
	if err := ValidateSuccess(mustJSON(t, doc), badJob); err == nil {
		t.Fatal("ValidateSuccess() accepted a malformed series UID")
	}
}

func TestValidateSuccessRejectsNumberOverflow(t *testing.T) {
	doc := validSuccessDocument()
	doc["candidates"].([]any)[0].(map[string]any)["score"] = json.RawMessage(`1e999`)
	// json.RawMessage inside a map marshals verbatim, producing a huge exponent.
	if err := ValidateSuccess(mustJSON(t, doc), testJob()); err == nil {
		t.Fatal("ValidateSuccess() accepted an overflowing score")
	}
	doc2 := validSuccessDocument()
	doc2["future_field"] = json.RawMessage(`1e999`)
	if err := ValidateSuccess(mustJSON(t, doc2), testJob()); err == nil {
		t.Fatal("ValidateSuccess() accepted an overflowing unknown field")
	}
}

func TestValidateSuccessRejectsTrailingJSON(t *testing.T) {
	raw := append(mustJSON(t, validSuccessDocument()), []byte(` {}`)...)
	if err := ValidateSuccess(raw, testJob()); err == nil {
		t.Fatal("ValidateSuccess() accepted trailing JSON content")
	}
}

func TestValidateSuccessRejectsDuplicateKeysRecursively(t *testing.T) {
	top := `{"schema_version":"1","schema_version":"1","series_instance_uid":"` + testSeriesUID +
		`","frame_of_reference_uid":"` + testFrameUID + `","coordinate_system":"DICOM_LPS_mm","model":{},"input":{},"candidates":[],"timing_s":{},"resources":{}}`
	if err := ValidateSuccess(json.RawMessage(top), testJob()); err == nil {
		t.Fatal("ValidateSuccess() accepted duplicate top-level keys")
	}
	nested := `{"schema_version":"1","series_instance_uid":"` + testSeriesUID +
		`","frame_of_reference_uid":"` + testFrameUID + `","coordinate_system":"DICOM_LPS_mm",` +
		`"model":{"bundle":"x","bundle":"y"},"input":{},"candidates":[],"timing_s":{},"resources":{}}`
	if err := ValidateSuccess(json.RawMessage(nested), testJob()); err == nil {
		t.Fatal("ValidateSuccess() accepted duplicate nested keys")
	}
}

func TestValidateFailureAcceptsAllowlistedCode(t *testing.T) {
	for _, code := range []string{"BUNDLE_INVALID", "MODEL_LOAD_FAILED", "INVALID_DICOM", "GEOMETRY_MISMATCH", "MODEL_OOM"} {
		raw := mustJSON(t, map[string]any{
			"schema_version": "1",
			"error":          map[string]any{"code": code, "message": "fixed safe description"},
		})
		got, err := ValidateFailure(raw)
		if err != nil {
			t.Fatalf("ValidateFailure(%s) error = %v, want nil", code, err)
		}
		if got != code {
			t.Fatalf("ValidateFailure() code = %q, want %q", got, code)
		}
	}
}

func TestValidateFailureNeverReturnsMessage(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"schema_version": "1",
		"error":          map[string]any{"code": "BUNDLE_INVALID", "message": testSecretPath},
	})
	code, err := ValidateFailure(raw)
	if err != nil {
		t.Fatalf("ValidateFailure() error = %v, want nil", err)
	}
	if strings.Contains(code, testSecretPath) {
		t.Fatalf("ValidateFailure() returned the untrusted message in the code %q", code)
	}
}

func TestValidateFailureRejectsMalformedEnvelope(t *testing.T) {
	cases := map[string]func(doc map[string]any){
		"wrong schema version": func(d map[string]any) { d["schema_version"] = "2" },
		"unknown code":         func(d map[string]any) { d["error"].(map[string]any)["code"] = "TOTALLY_UNKNOWN" },
		"empty code":           func(d map[string]any) { d["error"].(map[string]any)["code"] = "" },
		"empty message":        func(d map[string]any) { d["error"].(map[string]any)["message"] = "" },
		"missing message":      func(d map[string]any) { delete(d["error"].(map[string]any), "message") },
		"extra error key":      func(d map[string]any) { d["error"].(map[string]any)["detail"] = "x" },
		"extra top-level key":  func(d map[string]any) { d["model"] = map[string]any{} },
		"error not object":     func(d map[string]any) { d["error"] = "boom" },
		"code not string":      func(d map[string]any) { d["error"].(map[string]any)["code"] = 1.0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := map[string]any{
				"schema_version": "1",
				"error":          map[string]any{"code": "BUNDLE_INVALID", "message": testSecretPath},
			}
			mutate(doc)
			raw := mustJSON(t, doc)
			_, err := ValidateFailure(raw)
			if err == nil {
				t.Fatalf("ValidateFailure() accepted a malformed envelope")
			}
			if strings.Contains(err.Error(), testSecretPath) {
				t.Fatalf("ValidateFailure() leaked the untrusted message: %v", err)
			}
		})
	}
}

func TestValidateFailureRejectsSuccessPayload(t *testing.T) {
	raw := mustJSON(t, validSuccessDocument())
	if _, err := ValidateFailure(raw); err == nil {
		t.Fatal("ValidateFailure() accepted a success document")
	}
}

func TestValidateStderrTailSanitization(t *testing.T) {
	cases := map[string]string{
		"":                                      "",
		"infer: MODEL_OOM\n":                    "infer: MODEL_OOM",
		"infer: MODEL_OOM\n/home/secret/path\n": "infer: MODEL_OOM\n[redacted]",
		"infer: NOT_A_CODE\n":                   "[redacted]",
		"infer: MODEL_OOM extra\n":              "[redacted]",
		"partial-secret infer: NOT_CT\ninfer: NOT_CT": "[redacted]\ninfer: NOT_CT",
		"Traceback (most recent call last):\n":        "[redacted]",
	}
	for input, want := range cases {
		if got := SanitizeStderrTail(input); got != want {
			t.Fatalf("SanitizeStderrTail(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateStderrTailIsBoundedAndValidUTF8(t *testing.T) {
	// A 4 KiB tail full of short non-matching lines expands when replaced by
	// "[redacted]", so the result must still be capped and UTF-8 valid.
	var builder []byte
	for i := 0; i < 3000; i++ {
		builder = append(builder, "infer: SECRET_MESSAGE_0123456789\n"...)
	}
	out := SanitizeStderrTail(string(builder))
	if len(out) > MaxStderrTailBytes {
		t.Fatalf("SanitizeStderrTail length = %d, want <= %d", len(out), MaxStderrTailBytes)
	}
	if !utf8.ValidString(out) {
		t.Fatal("SanitizeStderrTail produced invalid UTF-8")
	}
	if strings.Contains(out, "SECRET_MESSAGE") {
		t.Fatalf("SanitizeStderrTail leaked content: %q", out)
	}
}

func TestValidateCLISafeCodeSet(t *testing.T) {
	if !IsCLISafeCode("MODEL_OOM") || !IsCLISafeCode("GEOMETRY_MISMATCH") {
		t.Fatal("IsCLISafeCode rejected known CLI codes")
	}
	if IsCLISafeCode("TOTALLY_UNKNOWN") || IsCLISafeCode("") {
		t.Fatal("IsCLISafeCode accepted an unknown code")
	}
}

func TestValidateCodeErrorCarriesCategory(t *testing.T) {
	err := error(&CodeError{Code: CodeAITimeout, StderrTail: "infer: MODEL_OOM"})
	var target *CodeError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to unwrap a CodeError")
	}
	if target.Code != CodeAITimeout || target.StderrTail != "infer: MODEL_OOM" {
		t.Fatalf("CodeError = %+v", target)
	}
	if err.Error() != CodeAITimeout {
		t.Fatalf("CodeError.Error() = %q", err.Error())
	}
}
