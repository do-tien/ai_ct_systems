package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sentinelSecret = "Sentinel-Patient-Name-Doe"
	sentinelUID    = "1.2.840.113619.2.999.9999.99999"
)

// baseEnv sets a complete but unreachable dependency configuration so a run
// fails after argument parsing without contacting a real service.
func baseEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"POSTGRES_HOST":     "127.0.0.1",
		"POSTGRES_PORT":     "1",
		"POSTGRES_DB":       "nodule",
		"POSTGRES_USER":     "nodule",
		"POSTGRES_PASSWORD": sentinelSecret,
		"ORTHANC_URL":       "http://127.0.0.1:1",
		"ORTHANC_USER":      "nodule",
		"ORTHANC_PASSWORD":  sentinelSecret,
		"HTTP_ADDR":         "127.0.0.1:0",
	} {
		t.Setenv(name, value)
	}
}

// dicomDir creates a directory with two non-empty files.
func dicomDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if name != "" {
		dir = filepath.Join(dir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for _, file := range []string{"a.dcm", "b.dcm"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("synthetic"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return dir
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	dir := dicomDir(t, "")
	cases := []struct {
		name string
		args []string
	}{
		{"missing_dir", nil},
		{"empty_dir", []string{"--dir", ""}},
		{"unknown_flag", []string{"--dir", dir, "--nope"}},
		{"positional_argument", []string{"--dir", dir, "extra"}},
		{"invalid_source", []string{"--dir", dir, "--source", "has space"}},
		{"long_source", []string{"--dir", dir, "--source", strings.Repeat("x", 65)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "usage: importer") {
				t.Fatalf("stderr = %q, want fixed usage", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if strings.Contains(stderr.String(), dir) {
				t.Fatalf("stderr echoed a local path: %q", stderr.String())
			}
		})
	}
}

func TestRunDoesNotEchoUserPath(t *testing.T) {
	baseEnv(t)
	secretDir := filepath.Join(t.TempDir(), sentinelSecret, "series")
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--dir", secretDir, "--source", "invalid label"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr %q)", code, stderr.String())
	}
	if strings.Contains(stderr.String(), sentinelSecret) || strings.Contains(stdout.String(), sentinelSecret) {
		t.Fatalf("output echoed the operator path: stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestRunRedactsDependencyFailures(t *testing.T) {
	baseEnv(t)
	dir := dicomDir(t, "")
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"--dir", dir, "--source", "local"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr.String())
	}
	combined := stdout.String() + stderr.String()
	for _, secret := range []string{sentinelSecret, sentinelUID, "127.0.0.1:1", dir} {
		if strings.Contains(combined, secret) {
			t.Fatalf("output leaked %q: %q", secret, combined)
		}
	}
	if !strings.Contains(stderr.String(), "importer failed") {
		t.Fatalf("stderr = %q, want a fixed failure category", stderr.String())
	}
}

func TestRunRejectsMissingConfiguration(t *testing.T) {
	for _, name := range []string{"POSTGRES_HOST", "ORTHANC_URL"} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(name, "")
			dir := dicomDir(t, "")
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), []string{"--dir", dir}, &stdout, &stderr); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if strings.Contains(stderr.String(), sentinelSecret) {
				t.Fatalf("stderr leaked a credential: %q", stderr.String())
			}
		})
	}
}

func TestRunHonorsCancellation(t *testing.T) {
	baseEnv(t)
	dir := dicomDir(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"--dir", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr.String())
	}
}
