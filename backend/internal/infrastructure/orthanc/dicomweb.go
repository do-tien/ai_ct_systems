// Package orthanc provides the Orthanc integration adapter.
package orthanc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// dicomwebRequestTimeout bounds a whole image request, including streaming the
// response back to the browser. It is deliberately separate from the health
// probe's three-second budget.
const dicomwebRequestTimeout = 2 * time.Minute

// dicomwebFixedWarning replaces any upstream Warning: the upstream text and
// warn-agent can name an internal authority, so only a fixed, nonidentifying
// value is forwarded.
const dicomwebFixedWarning = `199 nodule-reader "Upstream warning suppressed"`

// successHeaders are the only end-to-end headers preserved from a successful
// upstream image response. Everything else (Server, Location, Set-Cookie,
// authentication, forwarding and arbitrary diagnostics) is dropped.
var successHeaders = map[string]bool{
	"Content-Type":     true,
	"Content-Length":   true,
	"Content-Encoding": true,
	"Content-Range":    true,
	"Accept-Ranges":    true,
	"Etag":             true, // canonical form of ETag
	"Last-Modified":    true,
	"Vary":             true,
}

// NewDICOMwebProxy builds the authenticated, read-only streaming proxy that
// forwards permitted requests to Orthanc's DICOMweb endpoint. It is intended to
// sit behind the transport gate, which is the only component that decides which
// requests may arrive here; the proxy still sanitizes credentials and failures
// so no upstream detail reaches the browser. The supplied transport is owned by
// the caller; a nil transport falls back to a documented default clone.
func NewDICOMwebProxy(baseURL string, user string, password string, transport http.RoundTripper) (http.Handler, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse orthanc dicomweb base URL")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("orthanc dicomweb base URL must use http or https")
	}
	if target.Host == "" {
		return nil, fmt.Errorf("orthanc dicomweb base URL is missing a host")
	}
	basePath := strings.TrimRight(target.Path, "/")

	if transport == nil {
		transport = defaultDICOMwebTransport()
	}

	proxy := &dicomwebProxy{basePath: basePath}
	reverse := &httputil.ReverseProxy{
		Transport: transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			proxy.handleError(writer, request, err)
		},
		ModifyResponse: func(response *http.Response) error {
			proxy.sanitizeResponse(response)
			return nil
		},
		Rewrite: func(request *httputil.ProxyRequest) {
			proxy.rewrite(request, target, user, password)
		},
	}
	proxy.reverse = reverse
	return proxy, nil
}

// defaultDICOMwebTransport mirrors the lifecycle the composition root builds:
// bounded connect and response-header timeouts, no automatic compression and no
// retries.
func defaultDICOMwebTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
	}
}

type dicomwebProxy struct {
	reverse  *httputil.ReverseProxy
	basePath string
}

func (p *dicomwebProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), dicomwebRequestTimeout)
	defer cancel()

	tracked := &trackingWriter{ResponseWriter: writer}
	p.reverse.ServeHTTP(tracked, request.WithContext(ctx))
}

// rewrite pins the upstream scheme/authority/path and rebuilds the outbound
// header set. It never trusts a client-supplied Authorization, cookie or
// forwarding header, and it derives the public forwarded authority only from
// the validated incoming host.
func (p *dicomwebProxy) rewrite(request *httputil.ProxyRequest, target *url.URL, user string, password string) {
	out := *request.In.URL
	out.Scheme = target.Scheme
	out.Host = target.Host
	out.Path = p.basePath + request.In.URL.Path
	out.RawPath = ""
	if escaped := request.In.URL.EscapedPath(); escaped != request.In.URL.Path {
		out.RawPath = p.basePath + escaped
	}
	// Preserve the raw query byte-for-byte; never rebuild it through a map.
	out.RawQuery = request.In.URL.RawQuery
	out.ForceQuery = request.In.URL.ForceQuery
	out.Opaque = ""
	out.Fragment = ""
	request.Out.URL = &out
	request.Out.Host = target.Host

	header := http.Header{}
	for _, name := range []string{"Accept", "Accept-Encoding", "Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		for _, value := range request.In.Header.Values(name) {
			header.Add(name, value)
		}
	}
	header.Set("X-Forwarded-Host", request.In.Host)
	header.Set("X-Forwarded-Proto", publicScheme(request.In))
	request.Out.Header = header
	request.Out.SetBasicAuth(user, password)
}

// sanitizeResponse streams a successful representation unchanged and replaces
// every upstream failure with a local envelope. It runs before any header is
// written, so a replaced body cannot be mistaken for image bytes.
func (p *dicomwebProxy) sanitizeResponse(response *http.Response) {
	if (response.StatusCode >= 200 && response.StatusCode < 300) || response.StatusCode == http.StatusNotModified {
		header := http.Header{}
		for name, values := range response.Header {
			if successHeaders[http.CanonicalHeaderKey(name)] {
				for _, value := range values {
					header.Add(name, value)
				}
			}
		}
		header.Set("Cache-Control", "no-store")
		if len(response.Header.Values("Warning")) > 0 {
			header.Set("Warning", dicomwebFixedWarning)
		}
		response.Header = header
		// Trailer fields bypass the whitelist above: ReverseProxy copies them
		// straight to the client, so an upstream trailer could name an internal
		// authority. Drop the announcement now and strip the values the
		// transport re-populates when the body reaches EOF.
		response.Trailer = nil
		if response.Body != nil {
			response.Body = &trailerStrippingBody{body: response.Body, response: response}
		}
		return
	}

	status := http.StatusBadGateway
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusNotAcceptable, http.StatusRequestedRangeNotSatisfiable:
		status = response.StatusCode
	}
	replaceWithError(response, status)
}

// handleError reports a pre-header failure as 502/504. Failures after a header
// has been sent cannot become a clean JSON error or a different status: the
// incomplete stream is left to abort and nothing is appended.
func (p *dicomwebProxy) handleError(writer http.ResponseWriter, request *http.Request, err error) {
	if tracked, ok := writer.(*trackingWriter); ok && tracked.wroteHeader {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	status := http.StatusBadGateway
	if isTimeout(err) {
		status = http.StatusGatewayTimeout
	}
	writeDICOMwebError(writer, request, status)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// replaceWithError swaps an upstream failure representation for the local error
// envelope, discarding the upstream content headers, length, trailers and body.
func replaceWithError(response *http.Response, status int) {
	body := dicomwebEnvelope()
	if response.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		_ = response.Body.Close()
	}

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Header = header
	response.StatusCode = status
	response.Status = strconv.Itoa(status) + " " + http.StatusText(status)
	response.ContentLength = int64(len(body))
	response.Trailer = nil
	response.TransferEncoding = nil

	if response.Request != nil && response.Request.Method == http.MethodHead {
		response.Body = http.NoBody
		return
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
}

// trailerStrippingBody forwards an upstream body while ensuring the transport's
// EOF-time trailer population cannot reach the client.
type trailerStrippingBody struct {
	body     io.ReadCloser
	response *http.Response
}

func (b *trailerStrippingBody) Read(payload []byte) (int, error) {
	count, err := b.body.Read(payload)
	if err == io.EOF {
		b.response.Trailer = nil
	}
	return count, err
}

func (b *trailerStrippingBody) Close() error { return b.body.Close() }

// trackingWriter records whether a response header has been written so a
// post-header failure can be told apart from a pre-header one.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *trackingWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// Drop informational interim responses: the reverse proxy forwards
		// their headers before this call, and Go clears them right after, so
		// suppressing the status line keeps an upstream 1xx from leaking a
		// diagnostic header (or from marking the response as started).
		return
	}
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(body)
}

func (w *trackingWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// publicScheme derives the public scheme from the actual incoming TLS state; it
// ignores every client-supplied forwarding header.
func publicScheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	return "http"
}

// dicomwebEnvelope is the fixed local error body. Its shape and wording are
// stable so no upstream detail can leak through.
func dicomwebEnvelope() []byte {
	body, err := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: "dicomweb_unavailable", Message: "DICOM image request could not be completed."}})
	if err != nil {
		return []byte(`{"error":{"code":"dicomweb_unavailable","message":"DICOM image request could not be completed."}}`)
	}
	return body
}

// writeDICOMwebError writes the local envelope for a pre-header failure. HEAD
// advertises the local representation length but no body.
func writeDICOMwebError(writer http.ResponseWriter, request *http.Request, status int) {
	body := dicomwebEnvelope()
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(body)
	}
}
