package orthanc

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testTransport() *http.Transport {
	return &http.Transport{DisableCompression: true}
}

func newTestProxy(t *testing.T, baseURL string, transport http.RoundTripper) http.Handler {
	t.Helper()
	handler, err := NewDICOMwebProxy(baseURL, "orthanc-user", "orthanc-secret", transport)
	if err != nil {
		t.Fatalf("NewDICOMwebProxy: %v", err)
	}
	return handler
}

func testClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableCompression: true}, Timeout: 10 * time.Second}
}

func doRequest(t *testing.T, client *http.Client, method, target string, headers map[string]string, host string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if host != "" {
		request.Host = host
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return response, body
}

func TestDICOMwebMultipartAndNegotiation(t *testing.T) {
	part1 := bytes.Repeat([]byte{0x11}, 64)
	part2 := make([]byte, 70*1024) // larger than the health probe's 64 KiB limit
	for index := range part2 {
		part2[index] = byte(index % 251)
	}

	const boundary = "testboundary123"
	var encoded bytes.Buffer
	writePart := func(part []byte) {
		encoded.WriteString("--" + boundary + "\r\nContent-Type: application/dicom\r\n\r\n")
		encoded.Write(part)
		encoded.WriteString("\r\n")
	}
	writePart(part1)
	writePart(part2)
	encoded.WriteString("--" + boundary + "--\r\n")
	multipartBody := encoded.Bytes()

	var receivedAccept, receivedQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAccept = request.Header.Get("Accept")
		receivedQuery = request.URL.RawQuery
		writer.Header().Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+boundary)
		writer.Header().Set("Content-Length", strconv.Itoa(len(multipartBody)))
		_, _ = writer.Write(multipartBody)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, upstream.URL, testTransport())
	downstream := httptest.NewServer(proxy)
	defer downstream.Close()

	const wantQuery = "includefield=00080060&includefield=00080061&offset=0&limit=2"
	target := downstream.URL + "/dicom-web/studies/1.2.840.1/series/1.2.840.2/instances/1.2.840.3/frames/1?" + wantQuery
	response, body := doRequest(t, testClient(), http.MethodGet, target, map[string]string{
		"Accept": `multipart/related; type="application/dicom"; transfer-syntax=1.2.840.10008.1.2.1`,
	}, "localhost:5173")

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", response.StatusCode)
	}
	if receivedAccept != `multipart/related; type="application/dicom"; transfer-syntax=1.2.840.10008.1.2.1` {
		t.Errorf("upstream Accept not preserved: %q", receivedAccept)
	}
	if receivedQuery != wantQuery {
		t.Errorf("upstream raw query: want %q, got %q", wantQuery, receivedQuery)
	}
	if got := response.Header.Get("Content-Type"); got != `multipart/related; type="application/dicom"; boundary=`+boundary {
		t.Errorf("downstream Content-Type not preserved: %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control: want no-store, got %q", got)
	}
	if got := len(body); got != len(multipartBody) {
		t.Fatalf("body length: want %d, got %d", len(multipartBody), got)
	}

	_, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	first, err := reader.NextPart()
	if err != nil {
		t.Fatalf("first part: %v", err)
	}
	firstBytes, _ := io.ReadAll(first)
	if !bytes.Equal(firstBytes, part1) {
		t.Errorf("first part bytes differ")
	}
	second, err := reader.NextPart()
	if err != nil {
		t.Fatalf("second part: %v", err)
	}
	secondBytes, _ := io.ReadAll(second)
	if !bytes.Equal(secondBytes, part2) {
		t.Errorf("second part bytes differ (len %d)", len(secondBytes))
	}
}

func TestDICOMwebHeadAndPartialResponses(t *testing.T) {
	payload := []byte("0123456789")

	t.Run("HEAD mirrors headers without a body", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/dicom")
			writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			if request.Method == http.MethodHead {
				return
			}
			_, _ = writer.Write(payload)
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodHead, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status: want 200, got %d", response.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("HEAD body: want empty, got %q", body)
		}
		if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(payload)) {
			t.Errorf("HEAD Content-Length: want %d, got %q", len(payload), got)
		}
		if got := response.Header.Get("Content-Type"); got != "application/dicom" {
			t.Errorf("HEAD Content-Type: want application/dicom, got %q", got)
		}
	})

	t.Run("206 preserves selected bytes and Content-Range", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/dicom")
			writer.Header().Set("Content-Range", "bytes 2-5/10")
			writer.Header().Set("Accept-Ranges", "bytes")
			writer.WriteHeader(http.StatusPartialContent)
			_, _ = writer.Write(payload[2:6])
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", map[string]string{"Range": "bytes=2-5"}, "localhost")
		if response.StatusCode != http.StatusPartialContent {
			t.Fatalf("status: want 206, got %d", response.StatusCode)
		}
		if !bytes.Equal(body, payload[2:6]) {
			t.Errorf("206 body: want %q, got %q", payload[2:6], body)
		}
		if got := response.Header.Get("Content-Range"); got != "bytes 2-5/10" {
			t.Errorf("Content-Range: want bytes 2-5/10, got %q", got)
		}
	})

	t.Run("304 has no invented body", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("ETag", `"v1"`)
			writer.WriteHeader(http.StatusNotModified)
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", map[string]string{"If-None-Match": `"v1"`}, "localhost")
		if response.StatusCode != http.StatusNotModified {
			t.Fatalf("status: want 304, got %d", response.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("304 body: want empty, got %q", body)
		}
		if got := response.Header.Get("ETag"); got != `"v1"` {
			t.Errorf("ETag: want %q, got %q", `"v1"`, got)
		}
	})

	t.Run("compressed success preserves encoding and bytes", func(t *testing.T) {
		var compressed bytes.Buffer
		gzipWriter := gzip.NewWriter(&compressed)
		_, _ = gzipWriter.Write([]byte("compressed-image-payload"))
		_ = gzipWriter.Close()
		compressedBody := compressed.Bytes()

		var receivedEncoding string
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			receivedEncoding = request.Header.Get("Accept-Encoding")
			writer.Header().Set("Content-Type", "application/dicom")
			writer.Header().Set("Content-Encoding", "gzip")
			writer.Header().Set("Content-Length", strconv.Itoa(len(compressedBody)))
			_, _ = writer.Write(compressedBody)
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", map[string]string{"Accept-Encoding": "gzip"}, "localhost")
		if receivedEncoding != "gzip" {
			t.Errorf("upstream Accept-Encoding: want gzip, got %q", receivedEncoding)
		}
		if got := response.Header.Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("Content-Encoding: want gzip, got %q", got)
		}
		decoded, err := io.ReadAll(mustGzip(t, body))
		if err != nil {
			t.Fatalf("gunzip: %v", err)
		}
		if string(decoded) != "compressed-image-payload" {
			t.Errorf("decoded body: %q", decoded)
		}
	})
}

func mustGzip(t *testing.T, body []byte) io.Reader {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	return reader
}

func TestDICOMwebCredentialAndErrorBoundary(t *testing.T) {
	t.Run("hostile incoming headers cannot replace server auth or authority", func(t *testing.T) {
		var receivedAuth, receivedForwardedHost, receivedForwardedProto, receivedCookie, receivedForwarded string
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			receivedAuth = request.Header.Get("Authorization")
			receivedForwardedHost = request.Header.Get("X-Forwarded-Host")
			receivedForwardedProto = request.Header.Get("X-Forwarded-Proto")
			receivedCookie = request.Header.Get("Cookie")
			receivedForwarded = request.Header.Get("Forwarded")
			writer.Header().Set("Server", "Orthanc/1.13.0")
			writer.Header().Set("Set-Cookie", "session=leak")
			writer.Header().Set("Location", "http://internal-host:8042/secret")
			writer.Header().Set("Warning", `199 orthanc "http://internal-host:8042"`)
			writer.Header().Set("ETag", `"v2"`)
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, "ok")
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", map[string]string{
			"Authorization":    "Bearer attacker-token",
			"Cookie":           "session=attacker",
			"Forwarded":        "for=9.9.9.9",
			"X-Forwarded-Host": "evil.example",
			"X-Forwarded-For":  "9.9.9.9",
		}, "localhost:5173")

		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("orthanc-user:orthanc-secret"))
		if receivedAuth != wantAuth {
			t.Errorf("upstream Authorization: want server Basic auth, got %q", receivedAuth)
		}
		if receivedForwardedHost != "localhost:5173" {
			t.Errorf("upstream X-Forwarded-Host: want localhost:5173, got %q", receivedForwardedHost)
		}
		if receivedForwardedProto != "http" {
			t.Errorf("upstream X-Forwarded-Proto: want http, got %q", receivedForwardedProto)
		}
		if receivedCookie != "" {
			t.Errorf("client cookie was forwarded: %q", receivedCookie)
		}
		if receivedForwarded != "" {
			t.Errorf("client Forwarded header was forwarded: %q", receivedForwarded)
		}
		if body == nil || string(body) != "ok" {
			t.Errorf("success body: %q", body)
		}
		for _, name := range []string{"Server", "Set-Cookie", "Location"} {
			if value := response.Header.Get(name); value != "" {
				t.Errorf("diagnostic header %s leaked: %q", name, value)
			}
		}
		warning := response.Header.Get("Warning")
		if strings.Contains(warning, "internal-host") || strings.Contains(warning, "orthanc") {
			t.Errorf("Warning leaked upstream authority: %q", warning)
		}
		if got := response.Header.Get("ETag"); got != `"v2"` {
			t.Errorf("ETag: want %q, got %q", `"v2"`, got)
		}
	})

	t.Run("mapped upstream statuses use the local envelope", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			segments := strings.Split(request.URL.Path, "/")
			code, _ := strconv.Atoi(segments[len(segments)-1])
			writer.Header().Set("Content-Type", "text/plain")
			writer.Header().Set("X-Upstream-Secret", "internal")
			writer.WriteHeader(code)
			_, _ = io.WriteString(writer, "upstream-secret-body")
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		for _, code := range []int{400, 404, 406, 416} {
			response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/status/"+strconv.Itoa(code), nil, "localhost")
			if response.StatusCode != code {
				t.Errorf("status %d: want %d, got %d", code, code, response.StatusCode)
			}
			if strings.Contains(string(body), "upstream-secret-body") || response.Header.Get("X-Upstream-Secret") != "" {
				t.Errorf("status %d leaked upstream content: %q %q", code, body, response.Header.Get("X-Upstream-Secret"))
			}
			if !strings.Contains(string(body), `"dicomweb_unavailable"`) {
				t.Errorf("status %d missing local envelope: %q", code, body)
			}
		}
	})

	t.Run("auth failures and other upstream failures become 502", func(t *testing.T) {
		for _, code := range []int{401, 403, 500, 503} {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("WWW-Authenticate", `Basic realm="orthanc"`)
				writer.Header().Set("Content-Type", "text/plain")
				writer.WriteHeader(code)
				_, _ = io.WriteString(writer, "orthanc-internal-secret")
			}))
			downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
			response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
			downstream.Close()
			upstream.Close()
			if response.StatusCode != http.StatusBadGateway {
				t.Errorf("upstream %d: want 502, got %d", code, response.StatusCode)
			}
			if response.Header.Get("WWW-Authenticate") != "" {
				t.Errorf("upstream %d leaked WWW-Authenticate", code)
			}
			if strings.Contains(string(body), "orthanc-internal-secret") {
				t.Errorf("upstream %d leaked body: %q", code, body)
			}
		}
	})

	t.Run("redirects become 502 without contacting the target", func(t *testing.T) {
		var victimCalls int
		victim := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			victimCalls++
		}))
		defer victim.Close()
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Location", victim.URL+"/steal")
			writer.WriteHeader(http.StatusFound)
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status: want 502, got %d", response.StatusCode)
		}
		if response.Header.Get("Location") != "" {
			t.Errorf("Location leaked: %q", response.Header.Get("Location"))
		}
		if victimCalls != 0 {
			t.Errorf("redirect target was contacted %d times", victimCalls)
		}
		if !strings.Contains(string(body), `"dicomweb_unavailable"`) {
			t.Errorf("missing local envelope: %q", body)
		}
	})

	t.Run("refused connection becomes 502", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		closedURL := closed.URL
		closed.Close()
		downstream := httptest.NewServer(newTestProxy(t, closedURL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status: want 502, got %d", response.StatusCode)
		}
		if !strings.Contains(string(body), `"dicomweb_unavailable"`) {
			t.Errorf("missing local envelope: %q", body)
		}
	})

	t.Run("transport timeout becomes 504", func(t *testing.T) {
		downstream := httptest.NewServer(newTestProxy(t, "http://orthanc.invalid", timeoutTransport{}))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
		if response.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status: want 504, got %d", response.StatusCode)
		}
		if !strings.Contains(string(body), `"dicomweb_unavailable"`) {
			t.Errorf("missing local envelope: %q", body)
		}
	})

	t.Run("HEAD error is bodyless with local headers", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, "secret")
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodHead, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("status: want 502, got %d", response.StatusCode)
		}
		if len(body) != 0 {
			t.Errorf("HEAD error body: want empty, got %q", body)
		}
		if got := response.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("HEAD error Content-Type: want application/json, got %q", got)
		}
	})
}

type timeoutTransport struct{}

func (timeoutTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

func TestDICOMwebPublicMetadataURLs(t *testing.T) {
	t.Run("plugin-generated URLs use the incoming loopback authority", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			root := request.Header.Get("X-Forwarded-Proto") + "://" + request.Header.Get("X-Forwarded-Host") + "/dicom-web/studies/1.2.3"
			writer.Header().Set("Content-Type", "application/dicom+json")
			_, _ = io.WriteString(writer, `{"RetrieveURL":"`+root+`","BulkDataURI":"`+root+`/bulk/7FE00010"}`)
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.3/metadata", map[string]string{
			"X-Forwarded-Host":  "evil.example",
			"X-Forwarded-Proto": "https",
		}, "127.0.0.1:5173")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status: want 200, got %d", response.StatusCode)
		}
		if !strings.Contains(string(body), `http://127.0.0.1:5173/dicom-web/studies/1.2.3`) {
			t.Errorf("metadata URLs not same-origin: %q", body)
		}
		if strings.Contains(string(body), "evil.example") || strings.Contains(string(body), "https://") {
			t.Errorf("attacker forwarding values leaked into metadata: %q", body)
		}
	})

	t.Run("base path is prefixed and cannot escape the target", func(t *testing.T) {
		var receivedPath string
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			receivedPath = request.URL.Path
			_, _ = io.WriteString(writer, "ok")
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL+"/orthanc", testTransport()))
		defer downstream.Close()

		response, _ := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.3/series", nil, "localhost")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status: want 200, got %d", response.StatusCode)
		}
		if receivedPath != "/orthanc/dicom-web/studies/1.2.3/series" {
			t.Errorf("upstream path: want /orthanc/dicom-web/studies/1.2.3/series, got %q", receivedPath)
		}
	})
}

func TestDICOMwebSanitizesTrailersAndInterimHeaders(t *testing.T) {
	t.Run("upstream trailer cannot leak an internal authority", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/plain")
			writer.Header().Set("Trailer", "Warning")
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, "ok")
			writer.Header().Set("Warning", `199 orthanc "http://internal-host:8042"`)
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		response, body := doRequest(t, testClient(), http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", nil, "localhost")
		if string(body) != "ok" {
			t.Fatalf("body: %q", body)
		}
		for name, values := range response.Header {
			for _, value := range values {
				if strings.Contains(value, "internal-host") {
					t.Errorf("trailer leaked through header %s: %q", name, value)
				}
			}
		}
		if value := response.Trailer.Get("Warning"); strings.Contains(value, "internal-host") {
			t.Errorf("trailer leaked: %q", value)
		}
	})

	t.Run("interim informational headers never reach the client", func(t *testing.T) {
		downstream := httptest.NewServer(newTestProxy(t, "http://orthanc.invalid", interimTransport{}))
		defer downstream.Close()

		var interim []int
		trace := &httptrace.ClientTrace{
			Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
				interim = append(interim, code)
				return nil
			},
		}
		request, err := http.NewRequest(http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.840.1", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
		response, err := testClient().Do(request)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)

		if response.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("status %d body %q", response.StatusCode, body)
		}
		if len(interim) != 0 {
			t.Errorf("interim responses reached the client: %v", interim)
		}
		if got := response.Header.Get("Link"); got != "" {
			t.Errorf("interim header leaked: %q", got)
		}
	})
}

// interimTransport invokes the reverse proxy's informational-response trace
// hook before returning a normal 200, mimicking an upstream that emits an
// interim 1xx with a diagnostic header.
type interimTransport struct{}

func (interimTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if trace := httptrace.ContextClientTrace(request.Context()); trace != nil && trace.Got1xxResponse != nil {
		header := textproto.MIMEHeader{}
		header.Set("Link", "<http://internal-host:8042/secret>; rel=preload")
		if err := trace.Got1xxResponse(103, header); err != nil {
			return nil, err
		}
	}
	body := "ok"
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": {"text/plain"}, "Content-Length": {strconv.Itoa(len(body))}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       request,
	}, nil
}

func TestDICOMwebCancellationAndInterruptedStream(t *testing.T) {
	t.Run("client cancellation propagates upstream", func(t *testing.T) {
		cancelled := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/octet-stream")
			_, _ = writer.Write([]byte("x"))
			writer.(http.Flusher).Flush()
			select {
			case <-request.Context().Done():
				close(cancelled)
			case <-time.After(10 * time.Second):
			}
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		ctx, cancel := context.WithCancel(context.Background())
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL+"/dicom-web/studies/1.2.3", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		response, err := testClient().Do(request)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		buffer := make([]byte, 1)
		if _, err := io.ReadFull(response.Body, buffer); err != nil {
			t.Fatalf("read first chunk: %v", err)
		}
		cancel()
		response.Body.Close()

		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream request was not cancelled")
		}
	})

	t.Run("interrupted source stream aborts without appending JSON or logging URLs", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			connection, buffer, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 1000\r\n\r\n")
			buffer.Write([]byte("0123456789"))
			buffer.Flush()
			connection.Close()
		}))
		defer upstream.Close()
		downstream := httptest.NewServer(newTestProxy(t, upstream.URL, testTransport()))
		defer downstream.Close()

		var logs bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&logs)
		defer log.SetOutput(previous)

		// A mid-stream upstream failure cannot become a clean JSON error: the
		// reverse proxy aborts the incomplete stream. The client must observe a
		// failed transfer (either an early error or a short-body read error),
		// never a complete image ending in an error envelope.
		var body []byte
		response, err := testClient().Get(downstream.URL + "/dicom-web/studies/1.2.3")
		if err == nil {
			defer response.Body.Close()
			var readErr error
			body, readErr = io.ReadAll(response.Body)
			if readErr == nil {
				t.Errorf("expected the client to detect an incomplete transfer")
			}
		}
		if bytes.Contains(body, []byte(`"error"`)) || bytes.Contains(body, []byte("dicomweb_unavailable")) {
			t.Errorf("JSON error was appended to image bytes: %q", body)
		}
		upstreamHost := strings.TrimPrefix(upstream.URL, "http://")
		if strings.Contains(logs.String(), upstreamHost) || strings.Contains(logs.String(), "orthanc-secret") {
			t.Errorf("logs exposed an internal URL or secret: %q", logs.String())
		}
	})
}
