package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nodule-reader/backend/internal/application/health"
)

// gateTerminal records whether the request reached the injected upstream and
// answers 200 with a marker body, so gate tests can prove both reachability and
// non-reachability.
type gateTerminal struct {
	calls int
}

func (t *gateTerminal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.calls++
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "terminal")
}

const (
	validStudy    = "1.2.840.113619.2.55.3.1234567890.123.123456.1"
	validSeries   = "1.2.840.113619.2.55.3.1234567890.123.123456.2"
	validInstance = "1.2.840.113619.2.55.3.1234567890.123.123456.3"
	validWADO     = "/dicom-web/studies/" + validStudy + "/series/" + validSeries + "/instances/" + validInstance
)

func TestDICOMwebResourceGate(t *testing.T) {
	longUID := "1." + strings.Repeat("2", 63) // 65 chars, still syntactic digits/dots

	cases := []struct {
		name       string
		target     string
		host       string
		wantStatus int // 200 means the request reached the injected terminal
	}{
		// Permitted resources.
		{"qido studies", "/dicom-web/studies", "", http.StatusOK},
		{"qido series", "/dicom-web/series", "", http.StatusOK},
		{"qido instances", "/dicom-web/instances", "", http.StatusOK},
		{"qido series in study", "/dicom-web/studies/" + validStudy + "/series", "", http.StatusOK},
		{"qido instances in study", "/dicom-web/studies/" + validStudy + "/instances", "", http.StatusOK},
		{"qido instances in series", "/dicom-web/studies/" + validStudy + "/series/" + validSeries + "/instances", "", http.StatusOK},
		{"wado study", "/dicom-web/studies/" + validStudy, "", http.StatusOK},
		{"wado series", "/dicom-web/studies/" + validStudy + "/series/" + validSeries, "", http.StatusOK},
		{"wado instance", validWADO, "", http.StatusOK},
		{"study metadata", "/dicom-web/studies/" + validStudy + "/metadata", "", http.StatusOK},
		{"series metadata", "/dicom-web/studies/" + validStudy + "/series/" + validSeries + "/metadata", "", http.StatusOK},
		{"instance metadata", validWADO + "/metadata", "", http.StatusOK},
		{"single frame", validWADO + "/frames/1", "", http.StatusOK},
		{"frame list", validWADO + "/frames/1,2,3", "", http.StatusOK},
		{"bulk pixel data", validWADO + "/bulk/7FE00010", "", http.StatusOK},
		{"bulk lowercase tag", validWADO + "/bulk/7fe00010", "", http.StatusOK},
		{"bulk sequence item", validWADO + "/bulk/00420011/00081155/1/7FE00010", "", http.StatusOK},
		{"trailing slash on resource", "/dicom-web/studies/", "", http.StatusOK},
		{"trailing slash on instance", validWADO + "/", "", http.StatusOK},

		// Loopback authority forms.
		{"localhost authority", "/dicom-web/studies", "localhost", http.StatusOK},
		{"localhost with port", "/dicom-web/studies", "localhost:5173", http.StatusOK},
		{"ipv4 loopback", "/dicom-web/studies", "127.0.0.1", http.StatusOK},
		{"ipv4 loopback with port", "/dicom-web/studies", "127.0.0.1:8080", http.StatusOK},
		{"bracketed ipv6 loopback", "/dicom-web/studies", "[::1]", http.StatusOK},
		{"bracketed ipv6 loopback with port", "/dicom-web/studies", "[::1]:5173", http.StatusOK},

		// Malformed paths: 400 before any upstream contact.
		{"encoded slash", "/dicom-web/studies%2F" + validStudy, "", http.StatusBadRequest},
		{"encoded uppercase slash", "/dicom-web/studies%2F", "", http.StatusBadRequest},
		{"encoded backslash", "/dicom-web/studies%5C" + validStudy, "", http.StatusBadRequest},
		{"encoded dot", "/dicom-web/studies/%2e%2e/series", "", http.StatusBadRequest},
		{"raw backslash", "/dicom-web/studies\\" + validStudy, "", http.StatusBadRequest},
		{"double slash", "/dicom-web/studies//" + validStudy, "", http.StatusBadRequest},
		{"dot segment", "/dicom-web/./studies", "", http.StatusBadRequest},
		{"dotdot segment", "/dicom-web/studies/../series", "", http.StatusBadRequest},

		// Non-loopback authority: 400.
		{"external host", "/dicom-web/studies", "evil.example:80", http.StatusBadRequest},
		{"external ipv6", "/dicom-web/studies", "[2001:db8::1]:80", http.StatusBadRequest},
		{"missing port after colon", "/dicom-web/studies", "localhost:", http.StatusBadRequest},
		{"non numeric port", "/dicom-web/studies", "localhost:http", http.StatusBadRequest},
		{"port out of range", "/dicom-web/studies", "localhost:70000", http.StatusBadRequest},
		{"empty authority", "/dicom-web/studies", "-", http.StatusBadRequest},

		// Unknown resources: 404.
		{"namespace root", "/dicom-web", "", http.StatusNotFound},
		{"namespace root slash", "/dicom-web/", "", http.StatusNotFound},
		{"servers", "/dicom-web/servers", "", http.StatusNotFound},
		{"servers child", "/dicom-web/servers/orthanc", "", http.StatusNotFound},
		{"wado", "/dicom-web/wado", "", http.StatusNotFound},
		{"system", "/dicom-web/system", "", http.StatusNotFound},
		{"tools", "/dicom-web/tools", "", http.StatusNotFound},
		{"rendered", "/dicom-web/studies/" + validStudy + "/rendered", "", http.StatusNotFound},
		{"unexpected suffix", validWADO + "/nope", "", http.StatusNotFound},
		{"unknown study child", "/dicom-web/studies/" + validStudy + "/nope", "", http.StatusNotFound},
		{"frames after study", "/dicom-web/studies/" + validStudy + "/frames/1", "", http.StatusNotFound},
		{"bulk after study", "/dicom-web/studies/" + validStudy + "/bulk/7FE00010", "", http.StatusNotFound},
		{"leading zero uid", "/dicom-web/studies/01.2.3", "", http.StatusNotFound},
		{"empty uid component", "/dicom-web/studies/1..2", "", http.StatusNotFound},
		{"trailing dot uid", "/dicom-web/studies/1.2.3.", "", http.StatusNotFound},
		{"non numeric uid", "/dicom-web/studies/abc", "", http.StatusNotFound},
		{"uid too long", "/dicom-web/studies/" + longUID, "", http.StatusNotFound},
		{"single uid for instances", "/dicom-web/instances/" + validInstance, "", http.StatusNotFound},
		{"empty frame list", validWADO + "/frames/", "", http.StatusNotFound},
		{"zero frame", validWADO + "/frames/0", "", http.StatusNotFound},
		{"leading zero frame", validWADO + "/frames/01", "", http.StatusNotFound},
		{"empty frame component", validWADO + "/frames/1,,2", "", http.StatusNotFound},
		{"non numeric frame", validWADO + "/frames/x", "", http.StatusNotFound},
		{"bulk non tag", validWADO + "/bulk/xyz", "", http.StatusNotFound},
		{"bulk short tag", validWADO + "/bulk/7FE0", "", http.StatusNotFound},
		{"bulk ends with index", validWADO + "/bulk/7FE00010/1", "", http.StatusNotFound},
		{"bulk starts with index", validWADO + "/bulk/1/7FE00010", "", http.StatusNotFound},
		{"bulk empty", validWADO + "/bulk/", "", http.StatusNotFound},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			terminal := &gateTerminal{}
			handler := NewDICOMwebHandler(terminal)

			request := httptest.NewRequest(http.MethodGet, testCase.target, nil)
			request.Host = "localhost"
			if testCase.host != "" {
				if testCase.host == "-" {
					request.Host = ""
				} else {
					request.Host = testCase.host
				}
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status: want %d, got %d (body %q)", testCase.wantStatus, recorder.Code, recorder.Body.String())
			}
			if testCase.wantStatus == http.StatusOK {
				if terminal.calls != 1 {
					t.Errorf("expected terminal to be reached once, got %d", terminal.calls)
				}
				return
			}
			if terminal.calls != 0 {
				t.Errorf("denied request reached upstream %d times", terminal.calls)
			}
			if recorder.Body.Len() == 0 {
				t.Errorf("expected a local error envelope")
			}
		})
	}
}

func TestDICOMwebRouterRegistration(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		target     string
		wantStatus int
	}{
		{"namespace root reaches gate", http.MethodGet, "/dicom-web", http.StatusNotFound},
		{"permitted resource", http.MethodGet, "/dicom-web/studies", http.StatusOK},
		{"traversal not cleaned into a route", http.MethodGet, "/dicom-web/studies/../series", http.StatusBadRequest},
		{"double slash not redirected", http.MethodGet, "/dicom-web/studies//" + validStudy, http.StatusBadRequest},
		{"write blocked through router", http.MethodPost, "/dicom-web/studies", http.StatusMethodNotAllowed},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			terminal := &gateTerminal{}
			handler := NewRouter(
				health.NewService(stubProbe{}, stubProbe{}),
				nil, // unused initial-read dependency for DICOMweb tests
				nil, // unused final-read dependency for DICOMweb tests
				nil, // unused upload dependency for DICOMweb tests
				NewDICOMwebHandler(terminal),
			)
			request := httptest.NewRequest(testCase.method, testCase.target, nil)
			request.Host = "localhost"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status: want %d, got %d", testCase.wantStatus, recorder.Code)
			}
			wantCalls := 0
			if testCase.wantStatus == http.StatusOK {
				wantCalls = 1
			}
			if terminal.calls != wantCalls {
				t.Errorf("terminal calls: want %d, got %d", wantCalls, terminal.calls)
			}
		})
	}
}

func TestDICOMwebBlocksNonReadMethodsAndBodies(t *testing.T) {
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, "PROPFIND"}
	for _, method := range methods {
		t.Run("method "+method, func(t *testing.T) {
			terminal := &gateTerminal{}
			handler := NewDICOMwebHandler(terminal)

			request := httptest.NewRequest(method, "/dicom-web/studies/"+validStudy, strings.NewReader("payload"))
			request.Host = "localhost"
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status: want 405, got %d", recorder.Code)
			}
			if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow: want %q, got %q", "GET, HEAD", got)
			}
			if terminal.calls != 0 {
				t.Errorf("write method reached upstream %d times", terminal.calls)
			}
		})
	}

	t.Run("empty GET body is forwarded", func(t *testing.T) {
		terminal := &gateTerminal{}
		handler := NewDICOMwebHandler(terminal)
		request := httptest.NewRequest(http.MethodGet, "/dicom-web/studies", nil)
		request.Host = "localhost"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || terminal.calls != 1 {
			t.Fatalf("status %d, terminal calls %d", recorder.Code, terminal.calls)
		}
	})

	t.Run("GET with body", func(t *testing.T) {
		terminal := &gateTerminal{}
		handler := NewDICOMwebHandler(terminal)
		request := httptest.NewRequest(http.MethodGet, "/dicom-web/studies", strings.NewReader("payload"))
		request.Host = "localhost"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status: want 400, got %d", recorder.Code)
		}
		if terminal.calls != 0 {
			t.Errorf("GET-with-body reached upstream %d times", terminal.calls)
		}
	})

	t.Run("HEAD with chunked body", func(t *testing.T) {
		terminal := &gateTerminal{}
		handler := NewDICOMwebHandler(terminal)
		request := httptest.NewRequest(http.MethodHead, "/dicom-web/studies", strings.NewReader("payload"))
		request.Host = "localhost"
		request.ContentLength = -1
		request.TransferEncoding = []string{"chunked"}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status: want 400, got %d", recorder.Code)
		}
		if terminal.calls != 0 {
			t.Errorf("chunked HEAD reached upstream %d times", terminal.calls)
		}
	})

	t.Run("HEAD denial writes no body", func(t *testing.T) {
		terminal := &gateTerminal{}
		handler := NewDICOMwebHandler(terminal)
		request := httptest.NewRequest(http.MethodHead, "/dicom-web/servers", nil)
		request.Host = "localhost"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status: want 404, got %d", recorder.Code)
		}
		if recorder.Body.Len() != 0 {
			t.Errorf("HEAD error body: want empty, got %q", recorder.Body.String())
		}
	})
}
