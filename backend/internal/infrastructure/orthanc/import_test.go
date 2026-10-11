package orthanc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
)

const (
	testInstanceID = "aaaaaaaa-bbbbbbbb-cccccccc-dddddddd-eeeeeeee"
	testSeriesID   = "11111111-22222222-33333333-44444444-55555555"
	testStudyID    = "66666666-77777777-88888888-99999999-00000000"
	testInstance2  = "12345678-12345678-12345678-12345678-9abcdef0"
	testStudyUID   = "1.2.840.113619.2.55.3.604688.100.1"
	testSeriesUID  = "1.2.840.113619.2.55.3.604688.100.2"
	testFrameUID   = "1.2.840.113619.2.55.3.604688.100.3"
)

func newImportTestClient(serverURL string) *Client {
	return NewClient(NewImportHTTPClient(), serverURL, testUser, testPassword)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, body)
}

func uploadResponse(status, id, series, study string) string {
	return `{"ID":"` + id + `","ParentSeries":"` + series + `","ParentStudy":"` + study + `","Status":"` + status + `"}`
}

func TestImportUploadStreamsFileWithAuth(t *testing.T) {
	var (
		gotPath        string
		gotContentType string
		gotLength      int64
		gotBody        []byte
		gotUser        string
		gotPassword    string
		gotAuthOK      bool
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotLength = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		gotUser, gotPassword, gotAuthOK = r.BasicAuth()
		writeJSON(w, http.StatusOK, uploadResponse("Success", testInstanceID, testSeriesID, testStudyID))
	}))
	defer server.Close()

	payload := []byte("synthetic-dicom-payload-\x00\x01bytes")
	instance, err := newImportTestClient(server.URL).Upload(context.Background(), bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotPath != "/instances" {
		t.Fatalf("upload path = %q, want /instances", gotPath)
	}
	if gotContentType != "application/dicom" {
		t.Fatalf("content type = %q, want application/dicom", gotContentType)
	}
	if gotLength != int64(len(payload)) {
		t.Fatalf("content length = %d, want %d", gotLength, len(payload))
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("uploaded body altered: got %q", gotBody)
	}
	if !gotAuthOK || gotUser != testUser || gotPassword != testPassword {
		t.Fatalf("basic auth = %q/%q ok=%v", gotUser, gotPassword, gotAuthOK)
	}
	if instance.ID != testInstanceID || instance.SeriesID != testSeriesID || instance.StudyID != testStudyID {
		t.Fatalf("instance = %+v", instance)
	}
	if instance.AlreadyStored {
		t.Fatalf("AlreadyStored = true for Success status")
	}
}

func TestImportUploadResponseHandling(t *testing.T) {
	const secret = "patient-name-secret-body"
	cases := []struct {
		name       string
		statusCode int
		body       string
		wantErr    error
		stored     bool
	}{
		{"success", 200, uploadResponse("Success", testInstanceID, testSeriesID, testStudyID), nil, false},
		{"already_stored", 200, uploadResponse("AlreadyStored", testInstanceID, testSeriesID, testStudyID), nil, true},
		{"unknown_status", 200, uploadResponse("Failure", testInstanceID, testSeriesID, testStudyID), caseimport.ErrInvalidInput, false},
		{"empty_status", 200, `{"ID":"` + testInstanceID + `","ParentSeries":"` + testSeriesID + `","ParentStudy":"` + testStudyID + `","Status":""}`, caseimport.ErrInvalidInput, false},
		{"empty_id", 200, uploadResponse("Success", "", testSeriesID, testStudyID), caseimport.ErrInvalidInput, false},
		{"malformed_id", 200, uploadResponse("Success", "not-an-orthanc-id", testSeriesID, testStudyID), caseimport.ErrInvalidInput, false},
		{"missing_series", 200, `{"ID":"` + testInstanceID + `","ParentStudy":"` + testStudyID + `","Status":"Success"}`, caseimport.ErrInvalidInput, false},
		{"malformed_json", 200, `{"ID":`, caseimport.ErrInvalidInput, false},
		{"trailing_json", 200, uploadResponse("Success", testInstanceID, testSeriesID, testStudyID) + `{}`, caseimport.ErrInvalidInput, false},
		{"unauthorized", 401, secret, caseimport.ErrDependency, false},
		{"forbidden", 403, secret, caseimport.ErrDependency, false},
		{"server_error", 500, secret, caseimport.ErrDependency, false},
		{"redirect", 302, "", caseimport.ErrDependency, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.statusCode, tc.body)
			}))
			defer server.Close()

			instance, err := newImportTestClient(server.URL).Upload(context.Background(), strings.NewReader("body"), 4)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Upload: %v", err)
				}
				if instance.AlreadyStored != tc.stored {
					t.Fatalf("AlreadyStored = %v, want %v", instance.AlreadyStored, tc.stored)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked response body: %v", err)
			}
		})
	}
}

func TestImportUploadRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"Status":"`+strings.Repeat("a", 1<<20)+`"}`)
	}))
	defer server.Close()

	_, err := newImportTestClient(server.URL).Upload(context.Background(), strings.NewReader("body"), 4)
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
}

func TestImportUploadDoesNotFollowRedirectAcrossOrigins(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("second server received %s %s with auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		writeJSON(w, http.StatusOK, uploadResponse("Success", testInstanceID, testSeriesID, testStudyID))
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/instances", http.StatusFound)
	}))
	defer first.Close()

	_, err := newImportTestClient(first.URL).Upload(context.Background(), strings.NewReader("body"), 4)
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
}

func TestImportUploadHonorsCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := newImportTestClient(server.URL).Upload(ctx, strings.NewReader("body"), 4)
	cancel()
	close(release)
	server.Close()

	if !errors.Is(err, caseimport.ErrCanceled) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrCanceled)
	}
}

// importSeriesServer serves a minimal two-instance CT series and lets a test
// override individual endpoints.
func importSeriesServer(t *testing.T, instanceIDs []string, tagsFor func(id string) string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/instances", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, uploadResponse("Success", testInstanceID, testSeriesID, testStudyID))
	})
	mux.HandleFunc("/series/"+testSeriesID, func(w http.ResponseWriter, r *http.Request) {
		quoted := make([]string, len(instanceIDs))
		for i, id := range instanceIDs {
			quoted[i] = `"` + id + `"`
		}
		writeJSON(w, http.StatusOK, `{"ID":"`+testSeriesID+`","ParentStudy":"`+testStudyID+
			`","Instances":[`+strings.Join(quoted, ",")+`],"MainDicomTags":{"SeriesInstanceUID":"`+testSeriesUID+`"}}`)
	})
	mux.HandleFunc("/studies/"+testStudyID, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"ID":"`+testStudyID+`","MainDicomTags":{"StudyInstanceUID":"`+testStudyUID+`"}}`)
	})
	for _, id := range instanceIDs {
		mux.HandleFunc("/instances/"+id+"/simplified-tags", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, tagsFor(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/instances/"), "/simplified-tags")))
		})
	}
	return httptest.NewServer(mux)
}

func instanceTags(id, position string) string {
	return `{"SOPInstanceUID":"1.2.840.1.1.` + id[:4] + `","SOPClassUID":"1.2.840.10008.5.1.4.1.1.2",` +
		`"Modality":"CT","ImageType":["ORIGINAL","PRIMARY","AXIAL"],"NumberOfFrames":"1",` +
		`"FrameOfReferenceUID":"` + testFrameUID + `","SeriesInstanceUID":"` + testSeriesUID + `","StudyInstanceUID":"` + testStudyUID + `",` +
		`"Rows":"512","Columns":"512","PixelSpacing":"0.698\\0.698","ImageOrientationPatient":["1","0","0","0","1","0"],` +
		`"ImagePositionPatient":` + position + `,"SliceThickness":2.0}`
}

func TestDescribeSeriesParsesTypedMetadata(t *testing.T) {
	ids := []string{testInstanceID, testInstance2}
	server := importSeriesServer(t, ids, func(id string) string {
		if id == testInstance2 {
			return instanceTags(id, `["0","0","2.0"]`)
		}
		return instanceTags(id, `["0","0","0"]`)
	})
	defer server.Close()

	metadata, err := newImportTestClient(server.URL).DescribeSeries(context.Background(), testSeriesID)
	if err != nil {
		t.Fatalf("DescribeSeries: %v", err)
	}
	if metadata.SeriesID != testSeriesID || metadata.StudyID != testStudyID {
		t.Fatalf("ids = %+v", metadata)
	}
	if metadata.StudyInstanceUID != testStudyUID || metadata.SeriesInstanceUID != testSeriesUID {
		t.Fatalf("uids = %q/%q", metadata.StudyInstanceUID, metadata.SeriesInstanceUID)
	}
	if len(metadata.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(metadata.Instances))
	}
	first := metadata.Instances[0]
	if first.ID != testInstanceID || first.FrameOfReferenceUID != testFrameUID {
		t.Fatalf("first = %+v", first)
	}
	if first.PixelSpacing != [2]float64{0.698, 0.698} {
		t.Fatalf("pixel spacing = %v", first.PixelSpacing)
	}
	if first.ImageOrientationPatient != [6]float64{1, 0, 0, 0, 1, 0} {
		t.Fatalf("iop = %v", first.ImageOrientationPatient)
	}
	if first.ImagePositionPatient != [3]float64{0, 0, 0} {
		t.Fatalf("ipp = %v", first.ImagePositionPatient)
	}
	if first.NumberOfFrames != 1 || first.Rows != 512 || first.Columns != 512 || first.SliceThickness != 2.0 {
		t.Fatalf("scalars = %+v", first)
	}
	if len(first.ImageType) != 3 || first.ImageType[0] != "ORIGINAL" {
		t.Fatalf("image type = %v", first.ImageType)
	}
	if metadata.Instances[1].ImagePositionPatient[2] != 2.0 {
		t.Fatalf("second ipp = %v", metadata.Instances[1].ImagePositionPatient)
	}
}

func TestDescribeSeriesSupportsPrefixAndTrailingSlash(t *testing.T) {
	ids := []string{testInstanceID}
	var gotPath string
	inner := importSeriesServer(t, ids, func(id string) string { return instanceTags(id, `["0","0","0"]`) })
	defer inner.Close()

	// Wrap the series server under a path prefix.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		trimmed := strings.TrimPrefix(r.URL.Path, "/orthanc")
		r2 := r.Clone(r.Context())
		r2.URL.Path = trimmed
		client := &http.Client{}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+trimmed, r.Body)
		if err != nil {
			t.Errorf("proxy request: %v", err)
			return
		}
		request.Header = r.Header.Clone()
		response, err := client.Do(request)
		if err != nil {
			t.Errorf("proxy do: %v", err)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
	defer proxy.Close()

	metadata, err := newImportTestClient(proxy.URL+"/orthanc/").DescribeSeries(context.Background(), testSeriesID)
	if err != nil {
		t.Fatalf("DescribeSeries: %v", err)
	}
	if len(metadata.Instances) != 1 {
		t.Fatalf("instances = %d", len(metadata.Instances))
	}
	if !strings.HasPrefix(gotPath, "/orthanc/") {
		t.Fatalf("prefix path = %q", gotPath)
	}
}

func TestDescribeSeriesRejectsBadMetadata(t *testing.T) {
	const base = `"SOPInstanceUID":"1.2.3","SOPClassUID":"1.2.840.10008.5.1.4.1.1.2","Modality":"CT",` +
		`"ImageType":["ORIGINAL","PRIMARY","AXIAL"],"FrameOfReferenceUID":"` + testFrameUID + `",` +
		`"SeriesInstanceUID":"` + testSeriesUID + `","StudyInstanceUID":"` + testStudyUID + `",` +
		`"Rows":"512","Columns":"512","ImageOrientationPatient":["1","0","0","0","1","0"],` +
		`"SliceThickness":2.0,`
	cases := []struct {
		name string
		tags string
	}{
		{"missing_ipp", `{` + base + `"PixelSpacing":"0.7\\0.7"}`},
		{"missing_pixel_spacing", `{` + base + `"ImagePositionPatient":["0","0","0"]}`},
		{"missing_frame_uid", `{"SOPInstanceUID":"1.2.3","SOPClassUID":"1.2.840.10008.5.1.4.1.1.2","Modality":"CT","ImageType":["ORIGINAL"],"SeriesInstanceUID":"` + testSeriesUID + `","StudyInstanceUID":"` + testStudyUID + `","Rows":"512","Columns":"512","PixelSpacing":"0.7\\0.7","ImageOrientationPatient":["1","0","0","0","1","0"],"ImagePositionPatient":["0","0","0"],"SliceThickness":2.0}`},
		{"nonnumeric_spacing", `{` + base + `"PixelSpacing":"abc\\0.7","ImagePositionPatient":["0","0","0"]}`},
		{"nonfinite_spacing", `{` + base + `"PixelSpacing":"NaN\\0.7","ImagePositionPatient":["0","0","0"]}`},
		{"wrong_spacing_length", `{` + base + `"PixelSpacing":"0.7\\0.7\\0.7","ImagePositionPatient":["0","0","0"]}`},
		{"wrong_ipp_length", `{` + base + `"PixelSpacing":"0.7\\0.7","ImagePositionPatient":["0","0"]}`},
		{"wrong_ipp_type", `{` + base + `"PixelSpacing":"0.7\\0.7","ImagePositionPatient":{"z":"0"}}`},
		{"wrong_rows_type", `{"SOPInstanceUID":"1.2.3","SOPClassUID":"1.2.840.10008.5.1.4.1.1.2","Modality":"CT","ImageType":["ORIGINAL"],"FrameOfReferenceUID":"` + testFrameUID + `","SeriesInstanceUID":"` + testSeriesUID + `","StudyInstanceUID":"` + testStudyUID + `","Rows":["512"],"Columns":"512","PixelSpacing":"0.7\\0.7","ImageOrientationPatient":["1","0","0","0","1","0"],"ImagePositionPatient":["0","0","0"],"SliceThickness":2.0}`},
		{"series_uid_mismatch", `{` + base + `"PixelSpacing":"0.7\\0.7","ImagePositionPatient":["0","0","0"],"SeriesInstanceUID":"9.9.9"}`},
		{"empty_image_type", `{` + base + `"ImageType":"","PixelSpacing":"0.7\\0.7","ImagePositionPatient":["0","0","0"]}`},
		{"blank_image_type_component", `{` + base + `"ImageType":["ORIGINAL",""],"PixelSpacing":"0.7\\0.7","ImagePositionPatient":["0","0","0"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := importSeriesServer(t, []string{testInstanceID}, func(string) string { return tc.tags })
			defer server.Close()
			_, err := newImportTestClient(server.URL).DescribeSeries(context.Background(), testSeriesID)
			if !errors.Is(err, caseimport.ErrInvalidInput) {
				t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
			}
		})
	}
}

func TestDescribeSeriesRejectsStudyUIDMismatch(t *testing.T) {
	ids := []string{testInstanceID}
	server := importSeriesServer(t, ids, func(id string) string {
		return strings.Replace(instanceTags(id, `["0","0","0"]`), testStudyUID, "9.9.9", 1)
	})
	defer server.Close()

	_, err := newImportTestClient(server.URL).DescribeSeries(context.Background(), testSeriesID)
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
}

func TestDescribeSeriesRejectsInvalidSeriesID(t *testing.T) {
	server := importSeriesServer(t, []string{testInstanceID}, func(id string) string { return instanceTags(id, `["0","0","0"]`) })
	defer server.Close()

	_, err := newImportTestClient(server.URL).DescribeSeries(context.Background(), "../../etc/passwd")
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
}

func TestDescribeSeriesRejectsOversizedInventory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"Instances":["`+strings.Repeat("a", 4<<20)+`"]}`)
	}))
	defer server.Close()

	_, err := newImportTestClient(server.URL).DescribeSeries(context.Background(), testSeriesID)
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrInvalidInput)
	}
}

func TestListSeriesInstances(t *testing.T) {
	server := importSeriesServer(t, []string{testInstanceID, testInstance2}, func(id string) string { return instanceTags(id, `["0","0","0"]`) })
	defer server.Close()

	ids, err := newImportTestClient(server.URL).ListSeriesInstances(context.Background(), testSeriesID)
	if err != nil {
		t.Fatalf("ListSeriesInstances: %v", err)
	}
	if len(ids) != 2 || ids[0] != testInstanceID || ids[1] != testInstance2 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestListSeriesInstancesRejectsErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, "secret-body")
	}))
	defer server.Close()

	_, err := newImportTestClient(server.URL).ListSeriesInstances(context.Background(), testSeriesID)
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
	if strings.Contains(err.Error(), "secret-body") {
		t.Fatalf("error leaked body: %v", err)
	}

	_, err = newImportTestClient(server.URL).ListSeriesInstances(context.Background(), "bad-id")
	if !errors.Is(err, caseimport.ErrInvalidInput) {
		t.Fatalf("invalid id error = %v", err)
	}
}

// trackedReader records whether its Close method was called.
type trackedReader struct {
	reader io.Reader
	closed bool
}

func (t *trackedReader) Read(buffer []byte) (int, error) { return t.reader.Read(buffer) }

func (t *trackedReader) Close() error {
	t.closed = true
	return nil
}

func TestImportUploadDoesNotCloseCallerReader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, uploadResponse("Success", testInstanceID, testSeriesID, testStudyID))
	}))
	defer server.Close()

	body := &trackedReader{reader: strings.NewReader("payload")}
	if _, err := newImportTestClient(server.URL).Upload(context.Background(), body, 7); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if body.closed {
		t.Fatalf("Upload closed the caller's reader")
	}
}

func TestImportUploadClassifiesOversizedErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"`+strings.Repeat("a", 1<<20)+`"}`)
	}))
	defer server.Close()

	_, err := newImportTestClient(server.URL).Upload(context.Background(), strings.NewReader("body"), 4)
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
}

func TestDescribeSeriesClassifiesOversizedErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"`+strings.Repeat("a", 4<<20)+`"}`)
	}))
	defer server.Close()

	_, err := newImportTestClient(server.URL).DescribeSeries(context.Background(), testSeriesID)
	if !errors.Is(err, caseimport.ErrDependency) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
	}
}
