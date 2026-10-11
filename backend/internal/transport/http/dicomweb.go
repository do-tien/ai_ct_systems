// Package httpapi exposes the backend's HTTP transport.
package httpapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// dicomwebErrorEnvelope is the local, sanitized error body. It never carries
// upstream status text, credentials or internal addresses.
type dicomwebErrorEnvelope struct {
	Error dicomwebError `json:"error"`
}

type dicomwebError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// dicomwebPrefix is the browser-visible namespace for the read-only DICOMweb
// proxy. Everything under it is validated here before it can reach Orthanc.
const dicomwebPrefix = "/dicom-web"

// NewDICOMwebHandler wraps the upstream DICOMweb proxy with a read-only gate.
// The gate admits only syntactically valid GET/HEAD image resources on a
// loopback public authority; every other request is rejected locally, before
// the upstream is contacted. The upstream is never nil: the composition root
// always injects the real proxy.
func NewDICOMwebHandler(upstream http.Handler) http.Handler {
	return &dicomwebGate{upstream: upstream}
}

type dicomwebGate struct {
	upstream http.Handler
}

func (g *dicomwebGate) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !validPublicAuthority(request.Host) {
		writeDICOMwebError(writer, request, http.StatusBadRequest, "dicomweb_bad_request")
		return
	}
	if !validDICOMwebPath(request.URL) {
		writeDICOMwebError(writer, request, http.StatusBadRequest, "dicomweb_bad_request")
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writeDICOMwebError(writer, request, http.StatusMethodNotAllowed, "dicomweb_method_not_allowed")
		return
	}
	if request.ContentLength != 0 {
		writeDICOMwebError(writer, request, http.StatusBadRequest, "dicomweb_bad_request")
		return
	}
	if !dicomwebResourceAllowed(request.URL.Path) {
		writeDICOMwebError(writer, request, http.StatusNotFound, "dicomweb_not_found")
		return
	}
	g.upstream.ServeHTTP(writer, request)
}

// writeDICOMwebError emits the local error envelope. HEAD advertises the same
// local representation headers as GET but writes no body.
func writeDICOMwebError(writer http.ResponseWriter, request *http.Request, status int, code string) {
	body, err := json.Marshal(dicomwebErrorEnvelope{Error: dicomwebError{
		Code:    code,
		Message: "DICOM image request could not be completed.",
	}})
	if err != nil {
		// The envelope is a fixed literal shape; marshal cannot fail.
		writer.WriteHeader(status)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(status)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(body)
	}
}

// validPublicAuthority accepts the local-only deployment boundary: localhost,
// any loopback IP literal or the empty-host localhost forms, with an optional
// valid port. It rejects every other authority before any URL is generated.
func validPublicAuthority(authority string) bool {
	if authority == "" {
		return false
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.Index(authority, "]")
		if end < 0 {
			return false
		}
		hostname := authority[1:end]
		rest := authority[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") || !validPort(rest[1:]) {
				return false
			}
		}
		ip := net.ParseIP(hostname)
		return ip != nil && ip.IsLoopback()
	}
	if strings.Count(authority, ":") > 1 {
		// Unbracketed IPv6; a bare literal without a port is not a valid Host.
		return false
	}
	hostname := authority
	if index := strings.LastIndex(authority, ":"); index >= 0 {
		hostname = authority[:index]
		if !validPort(authority[index+1:]) {
			return false
		}
	}
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func validPort(port string) bool {
	if port == "" {
		return false
	}
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

// validDICOMwebPath rejects malformed escaping and traversal before routing.
// It inspects the escaped form so an encoded slash, backslash or dot cannot be
// silently decoded into a path separator or traversal segment.
func validDICOMwebPath(requestURL *url.URL) bool {
	escaped := strings.ToLower(requestURL.EscapedPath())
	for _, forbidden := range []string{"%2f", "%5c", "%2e"} {
		if strings.Contains(escaped, forbidden) {
			return false
		}
	}
	decoded := requestURL.Path
	if strings.Contains(decoded, "\\") || strings.Contains(decoded, "//") {
		return false
	}
	for _, segment := range strings.Split(decoded, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// dicomwebResourceAllowed matches the permitted resource grammar beneath the
// DICOMweb prefix, accepting at most one trailing slash.
func dicomwebResourceAllowed(path string) bool {
	if !strings.HasPrefix(path, dicomwebPrefix) {
		return false
	}
	rest := path[len(dicomwebPrefix):]
	if rest == "" || !strings.HasPrefix(rest, "/") {
		return false
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		return false
	}
	return matchDICOMwebSegments(strings.Split(rest[1:], "/"))
}

func matchDICOMwebSegments(segments []string) bool {
	switch segments[0] {
	case "studies":
		return matchStudySegments(segments[1:])
	case "series", "instances":
		return len(segments) == 1
	default:
		return false
	}
}

// matchStudySegments handles everything after "/studies".
func matchStudySegments(segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	if !isDICOMUID(segments[0]) {
		return false
	}
	return matchStudyResource(segments[1:])
}

func matchStudyResource(segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	switch segments[0] {
	case "metadata", "instances":
		return len(segments) == 1
	case "series":
		return matchSeriesSegments(segments[1:])
	default:
		return false
	}
}

// matchSeriesSegments handles everything after "/studies/{study}/series".
func matchSeriesSegments(segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	if !isDICOMUID(segments[0]) {
		return false
	}
	return matchSeriesResource(segments[1:])
}

func matchSeriesResource(segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	switch segments[0] {
	case "metadata":
		return len(segments) == 1
	case "instances":
		return matchInstanceSegments(segments[1:])
	default:
		return false
	}
}

// matchInstanceSegments handles everything after
// "/studies/{study}/series/{series}/instances".
func matchInstanceSegments(segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	if !isDICOMUID(segments[0]) {
		return false
	}
	return matchInstanceResource(segments[1:])
}

func matchInstanceResource(segments []string) bool {
	if len(segments) == 0 {
		return true
	}
	switch segments[0] {
	case "metadata":
		return len(segments) == 1
	case "frames":
		return len(segments) == 2 && validFrames(segments[1])
	case "bulk":
		return len(segments) >= 2 && validBulkPath(segments[1:])
	default:
		return false
	}
}

// isDICOMUID checks DICOM UID syntax: at most 64 ASCII characters, nonempty
// dot-separated decimal components, no leading zero in a multi-digit component.
func isDICOMUID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, component := range strings.Split(value, ".") {
		if component == "" {
			return false
		}
		if len(component) > 1 && component[0] == '0' {
			return false
		}
		for _, digit := range component {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

// validFrames accepts one or more comma-separated positive decimal frame
// numbers with no empty component and no leading zero.
func validFrames(value string) bool {
	for _, frame := range strings.Split(value, ",") {
		if frame == "" || frame[0] == '0' {
			return false
		}
		for _, digit := range frame {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

// validBulkPath accepts a metadata-advertised bulk path: a nonempty sequence of
// eight-hex-digit DICOM tags and nonnegative decimal sequence-item indices,
// starting and ending with a tag, with indices only between tags.
func validBulkPath(segments []string) bool {
	if !isHexTag(segments[0]) || !isHexTag(segments[len(segments)-1]) {
		return false
	}
	for index, segment := range segments {
		if isHexTag(segment) {
			continue
		}
		if !isDecimalIndex(segment) {
			return false
		}
		if index == 0 || index == len(segments)-1 {
			return false
		}
		if !isHexTag(segments[index-1]) || !isHexTag(segments[index+1]) {
			return false
		}
	}
	return true
}

func isHexTag(value string) bool {
	if len(value) != 8 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		case character >= 'A' && character <= 'F':
		default:
			return false
		}
	}
	return true
}

func isDecimalIndex(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
