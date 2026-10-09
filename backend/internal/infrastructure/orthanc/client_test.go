package orthanc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testUser     = "nodule"
	testPassword = "orthanc-dev-only"
)

func newTestClient(serverURL string, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	return NewClient(client, serverURL, testUser, testPassword)
}

func authorizedRequest(r *http.Request) bool {
	user, password, ok := r.BasicAuth()
	return ok && user == testUser && password == testPassword
}

func TestCheckAcceptsSystemVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorizedRequest(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/system" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Version":"26.9.1","Name":"orthanc"}`))
	}))
	defer server.Close()

	if err := newTestClient(server.URL, nil).Check(context.Background()); err != nil {
		t.Fatalf("expected healthy orthanc, got %v", err)
	}
}

func TestCheckNormalizesTrailingSlashInBaseURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/system" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Version":"26.9.1"}`))
	}))
	defer server.Close()

	if err := newTestClient(server.URL+"/", nil).Check(context.Background()); err != nil {
		t.Fatalf("expected trailing slash to be normalized, got %v", err)
	}
}

func TestCheckFailsOnRejectedAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := newTestClient(server.URL, nil).Check(context.Background()); err == nil {
		t.Fatal("expected 401 to be unhealthy")
	}
}

func TestCheckFailsOnUnexpectedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if err := newTestClient(server.URL, nil).Check(context.Background()); err == nil {
		t.Fatal("expected 500 to be unhealthy")
	}
}

func TestCheckFailsOnMalformedOrUnusablePayload(t *testing.T) {
	cases := map[string]string{
		"malformed json": `not json`,
		"empty version":  `{"Version":""}`,
		"wrong type":     `{"Version":123}`,
		"version object": `{"Version":{"major":1}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()

			if err := newTestClient(server.URL, nil).Check(context.Background()); err == nil {
				t.Fatalf("expected %s to be unhealthy", name)
			}
		})
	}
}

func TestCheckFailsWhenBodyExceedsLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Valid JSON prefix, but the oversized version string is truncated at the
		// 64 KiB read limit, leaving invalid JSON.
		_, _ = w.Write([]byte(`{"Version":"` + strings.Repeat("a", maxSystemBodyBytes+10) + `"}`))
	}))
	defer server.Close()

	if err := newTestClient(server.URL, nil).Check(context.Background()); err == nil {
		t.Fatal("expected an over-limit body to be unhealthy")
	}
}

func TestCheckHonorsRequestCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(release)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := newTestClient(server.URL, nil).Check(ctx); err == nil {
		t.Fatal("expected a cancelled request to be unhealthy")
	}
	select {
	case <-release:
	case <-time.After(time.Second):
		t.Error("expected the in-flight request to be cancelled")
	}
}
