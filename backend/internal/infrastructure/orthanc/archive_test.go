package orthanc

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func archiveClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	return NewArchiveClient(&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, server.URL, "user", "pass")
}

func TestArchiveDownloadStreamsBody(t *testing.T) {
	payload := []byte("PK\x03\x04 synthetic archive bytes")
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	var buffer bytes.Buffer
	if err := archiveClient(t, server).Download(context.Background(), testSeriesID, &buffer); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if gotPath != "/series/"+testSeriesID+"/archive" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("Authorization = %q, want Basic", gotAuth)
	}
	if !bytes.Equal(buffer.Bytes(), payload) {
		t.Fatalf("body = %q, want %q", buffer.Bytes(), payload)
	}
}

func TestArchiveDownloadRejectsInvalidIDWithoutRequest(t *testing.T) {
	hit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer server.Close()

	for _, id := range []string{"", "../../etc/passwd", "short-id", "0123abcd-4567ef89"} {
		if err := archiveClient(t, server).Download(context.Background(), id, io.Discard); err == nil {
			t.Fatalf("Download(%q) succeeded, want rejection", id)
		}
	}
	if hit {
		t.Fatal("server was contacted for an invalid series id")
	}
}

func TestArchiveDownloadRejectsRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	if err := archiveClient(t, server).Download(context.Background(), testSeriesID, io.Discard); err == nil {
		t.Fatal("Download() followed a redirect, want rejection")
	}
}

func TestArchiveDownloadRejectsNon200(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		if err := archiveClient(t, server).Download(context.Background(), testSeriesID, io.Discard); err == nil {
			t.Fatalf("Download() accepted status %d", status)
		}
		server.Close()
	}
}

func TestArchiveDownloadRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer server.Close()

	client := archiveClient(t, server)
	client.archiveLimit = 1024
	if err := client.Download(context.Background(), testSeriesID, io.Discard); err == nil {
		t.Fatal("Download() accepted a body over the cap")
	}
}

func TestArchiveDownloadRejectsTruncatedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer server.Close()

	if err := archiveClient(t, server).Download(context.Background(), testSeriesID, io.Discard); err == nil {
		t.Fatal("Download() accepted a truncated body")
	}
}

func TestArchiveDownloadHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- archiveClient(t, server).Download(ctx, testSeriesID, io.Discard)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Download() returned nil after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download() did not return after cancellation")
	}
}
