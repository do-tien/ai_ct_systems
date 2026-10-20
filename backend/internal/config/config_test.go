package config_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/config"
)

const testPassword = "s3cret-value"

func testPepper() string {
	return base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func setAuth(t *testing.T) {
	t.Helper()
	t.Setenv("AUTH_PIN_PEPPER", testPepper())
	t.Setenv("AUTH_COOKIE_SECURE", "")
}

func setPostgres(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"POSTGRES_HOST":     "127.0.0.1",
		"POSTGRES_PORT":     "5432",
		"POSTGRES_DB":       "nodule",
		"POSTGRES_USER":     "nodule",
		"POSTGRES_PASSWORD": testPassword,
	}
	for name, value := range overrides {
		base[name] = value
	}
	for name, value := range base {
		t.Setenv(name, value)
	}
}

func clearOrthanc(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ORTHANC_URL", "ORTHANC_USER", "ORTHANC_PASSWORD"} {
		t.Setenv(name, "")
	}
}

func TestLoadPostgresNeedsNoOrthanc(t *testing.T) {
	setPostgres(t, nil)
	clearOrthanc(t)

	settings, err := config.LoadPostgres()
	if err != nil {
		t.Fatalf("LoadPostgres() error = %v", err)
	}
	if settings.Host != "127.0.0.1" || settings.Port != 5432 || settings.Database != "nodule" ||
		settings.User != "nodule" || settings.Password != testPassword {
		t.Fatalf("LoadPostgres() = %+v, want parsed POSTGRES_* settings", settings)
	}
}

func TestLoadStillRequiresOrthanc(t *testing.T) {
	setPostgres(t, nil)
	setAuth(t)
	clearOrthanc(t)

	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "ORTHANC_URL") {
		t.Fatalf("Load() error = %v, want missing ORTHANC_URL", err)
	}
}

func TestLoadPostgresRejectsBadPorts(t *testing.T) {
	for _, port := range []string{"", "0", "65536", "not-a-number"} {
		t.Run(port, func(t *testing.T) {
			setPostgres(t, map[string]string{"POSTGRES_PORT": port})

			_, err := config.LoadPostgres()
			if err == nil || !strings.Contains(err.Error(), "POSTGRES_PORT") {
				t.Fatalf("LoadPostgres() error = %v, want POSTGRES_PORT error", err)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("error leaked the password: %v", err)
			}
		})
	}
}

func TestLoadPostgresNamesEveryMissingVariable(t *testing.T) {
	for _, name := range []string{"POSTGRES_HOST", "POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD"} {
		t.Run(name, func(t *testing.T) {
			setPostgres(t, map[string]string{name: ""})

			_, err := config.LoadPostgres()
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("LoadPostgres() error = %v, want %s error", err, name)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("error leaked the password: %v", err)
			}
		})
	}
}

func setOrthanc(t *testing.T) {
	t.Helper()
	t.Setenv("ORTHANC_URL", "http://127.0.0.1:8042")
	t.Setenv("ORTHANC_USER", "orthanc")
	t.Setenv("ORTHANC_PASSWORD", "orthanc-secret")
}

// setAI provides valid AI settings rooted in a temp directory, then applies
// overrides. An empty override unsets the variable so defaults can be checked.
func setAI(t *testing.T, overrides map[string]string) {
	t.Helper()
	root := t.TempDir()
	python := filepath.Join(root, "python.exe")
	if err := os.WriteFile(python, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write python stub: %v", err)
	}
	bundle := filepath.Join(root, "bundle")
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	values := map[string]string{
		"AI_PYTHON":     python,
		"AI_BUNDLE_DIR": bundle,
		"AI_JOB_DIR":    filepath.Join(root, "jobs"),
		"AI_TIMEOUT":    "",
		"AI_DEVICE":     "",
		"UPLOAD_DIR":    filepath.Join(root, "uploads"),
	}
	for name, value := range overrides {
		values[name] = value
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
}

func loadServerConfig(t *testing.T) (config.Config, error) {
	t.Helper()
	setPostgres(t, nil)
	setAuth(t)
	setOrthanc(t)
	return config.Load()
}

func TestLoadOrthancNeedsNoAI(t *testing.T) {
	setPostgres(t, nil)
	setOrthanc(t)
	for _, name := range []string{"AI_PYTHON", "AI_BUNDLE_DIR", "AI_JOB_DIR", "AI_TIMEOUT", "AI_DEVICE"} {
		t.Setenv(name, "")
	}
	settings, err := config.LoadOrthanc()
	if err != nil {
		t.Fatalf("LoadOrthanc() error = %v", err)
	}
	if settings.URL != "http://127.0.0.1:8042" || settings.User != "orthanc" {
		t.Fatalf("LoadOrthanc() = %+v", settings)
	}
}

func TestLoadAIDefaults(t *testing.T) {
	setAI(t, map[string]string{"AI_TIMEOUT": "", "AI_DEVICE": ""})
	cfg, err := loadServerConfig(t)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AITimeout != 15*time.Minute {
		t.Fatalf("AITimeout = %v, want 15m", cfg.AITimeout)
	}
	if cfg.AIDevice != "cuda" {
		t.Fatalf("AIDevice = %q, want cuda", cfg.AIDevice)
	}
}

func TestLoadAINamesEveryMissingVariable(t *testing.T) {
	for _, name := range []string{"AI_PYTHON", "AI_BUNDLE_DIR", "AI_JOB_DIR"} {
		t.Run(name, func(t *testing.T) {
			setAI(t, map[string]string{name: ""})
			_, err := loadServerConfig(t)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load() error = %v, want %s error", err, name)
			}
		})
	}
}

func TestLoadAIRejectsInvalidSettings(t *testing.T) {
	root := t.TempDir()
	regularFile := filepath.Join(root, "a-file")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cases := map[string]map[string]string{
		"python relative":      {"AI_PYTHON": "python.exe"},
		"python missing":       {"AI_PYTHON": filepath.Join(root, "absent.exe")},
		"python is directory":  {"AI_PYTHON": root},
		"bundle is file":       {"AI_BUNDLE_DIR": regularFile},
		"bundle relative":      {"AI_BUNDLE_DIR": "bundle"},
		"bundle missing":       {"AI_BUNDLE_DIR": filepath.Join(root, "absent")},
		"timeout missing unit": {"AI_TIMEOUT": "15"},
		"timeout zero":         {"AI_TIMEOUT": "0s"},
		"timeout negative":     {"AI_TIMEOUT": "-1m"},
		"device unknown":       {"AI_DEVICE": "gpu"},
		"device malformed":     {"AI_DEVICE": "cuda:"},
		"job dir relative":     {"AI_JOB_DIR": "jobs"},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			setAI(t, overrides)
			if _, err := loadServerConfig(t); err == nil {
				t.Fatalf("Load() accepted invalid settings %v", overrides)
			}
		})
	}
}

func TestLoadAICreatesJobDir(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "nested", "jobs")
	setAI(t, map[string]string{"AI_JOB_DIR": jobDir})
	cfg, err := loadServerConfig(t)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AIJobDir != jobDir {
		t.Fatalf("AIJobDir = %q", cfg.AIJobDir)
	}
	info, err := os.Stat(jobDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("job dir was not created: %v", err)
	}
}

func TestLoadAIRejectsJobDirOverlap(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	for name, jobDir := range map[string]string{
		"identical": bundle,
		"inside":    filepath.Join(bundle, "jobs"),
		"contains":  root,
	} {
		t.Run(name, func(t *testing.T) {
			setAI(t, map[string]string{"AI_BUNDLE_DIR": bundle, "AI_JOB_DIR": jobDir})
			if _, err := loadServerConfig(t); err == nil {
				t.Fatalf("Load() accepted overlapping job dir %q", jobDir)
			}
		})
	}
}

func TestLoadCreatesUploadDir(t *testing.T) {
	uploadDir := filepath.Join(t.TempDir(), "nested", "uploads")
	setAI(t, map[string]string{"UPLOAD_DIR": uploadDir})
	cfg, err := loadServerConfig(t)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.UploadDir != uploadDir {
		t.Fatalf("UploadDir = %q", cfg.UploadDir)
	}
	info, err := os.Stat(uploadDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("upload dir was not created: %v", err)
	}
}

func TestLoadRejectsInvalidUploadDir(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	jobDir := filepath.Join(root, "jobs")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	regularFile := filepath.Join(root, "file")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for name, uploadDir := range map[string]string{
		"relative":    "uploads",
		"is a file":   regularFile,
		"empty":       "",
		"in bundle":   filepath.Join(bundle, "uploads"),
		"in job dir":  filepath.Join(jobDir, "uploads"),
		"contains AI": root,
	} {
		t.Run(name, func(t *testing.T) {
			setAI(t, map[string]string{
				"AI_BUNDLE_DIR": bundle,
				"AI_JOB_DIR":    jobDir,
				"UPLOAD_DIR":    uploadDir,
			})
			if _, err := loadServerConfig(t); err == nil {
				t.Fatalf("Load() accepted upload dir %q", uploadDir)
			}
		})
	}
}

func TestLoadAuthNeedsNoOrthancOrAI(t *testing.T) {
	setPostgres(t, nil)
	setAuth(t)
	clearOrthanc(t)
	for _, name := range []string{
		"AI_PYTHON", "AI_BUNDLE_DIR", "AI_JOB_DIR", "AI_TIMEOUT", "AI_DEVICE",
		"UPLOAD_DIR", "HTTP_ADDR",
	} {
		t.Setenv(name, "")
	}

	settings, err := config.LoadAuth()
	if err != nil {
		t.Fatalf("LoadAuth() error = %v", err)
	}
	if settings.Postgres.Host != "127.0.0.1" || settings.Postgres.Port != 5432 ||
		settings.Postgres.Database != "nodule" || settings.Postgres.User != "nodule" ||
		settings.Postgres.Password != testPassword {
		t.Fatalf("LoadAuth() Postgres = %+v", settings.Postgres)
	}
	if settings.PINPepper != testPepper() {
		t.Fatal("LoadAuth() did not return the configured pepper")
	}
}

func TestLoadAuthRequiresValidPepperWithoutLeakingIt(t *testing.T) {
	cases := map[string]string{
		"missing":     "",
		"not base64":  "this is not base64url!",
		"short":       base64.RawURLEncoding.EncodeToString([]byte("too-short")),
		"long":        base64.RawURLEncoding.EncodeToString(make([]byte, 33)),
		"padded":      base64.URLEncoding.EncodeToString(make([]byte, 32)),
		"empty bytes": base64.RawURLEncoding.EncodeToString([]byte("")),
	}
	for name, pepper := range cases {
		t.Run(name, func(t *testing.T) {
			setPostgres(t, nil)
			t.Setenv("AUTH_PIN_PEPPER", pepper)

			_, err := config.LoadAuth()
			if err == nil || !strings.Contains(err.Error(), "AUTH_PIN_PEPPER") {
				t.Fatalf("LoadAuth() error = %v, want AUTH_PIN_PEPPER error", err)
			}
			if pepper != "" && strings.Contains(err.Error(), pepper) {
				t.Fatalf("error leaked the pepper value: %v", err)
			}
		})
	}
}

func TestLoadRequiresAuthPepper(t *testing.T) {
	setPostgres(t, nil)
	setOrthanc(t)
	setAI(t, nil)
	t.Setenv("AUTH_PIN_PEPPER", "")
	t.Setenv("AUTH_COOKIE_SECURE", "")

	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "AUTH_PIN_PEPPER") {
		t.Fatalf("Load() error = %v, want AUTH_PIN_PEPPER error", err)
	}
}

func TestLoadReadsAuthCookieSecure(t *testing.T) {
	cases := map[string]struct {
		value string
		want  bool
		ok    bool
	}{
		"default": {value: "", want: false, ok: true},
		"false":   {value: "false", want: false, ok: true},
		"true":    {value: "true", want: true, ok: true},
		"one":     {value: "1", ok: false},
		"yes":     {value: "yes", ok: false},
		"upper":   {value: "TRUE", ok: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			setPostgres(t, nil)
			setOrthanc(t)
			setAI(t, nil)
			t.Setenv("AUTH_PIN_PEPPER", testPepper())
			t.Setenv("AUTH_COOKIE_SECURE", tc.value)

			cfg, err := config.Load()
			if tc.ok {
				if err != nil {
					t.Fatalf("Load() error = %v", err)
				}
				if cfg.AuthCookieSecure != tc.want {
					t.Fatalf("AuthCookieSecure = %v, want %v", cfg.AuthCookieSecure, tc.want)
				}
				if cfg.AuthPinPepper != testPepper() {
					t.Fatal("AuthPinPepper not populated on Config")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "AUTH_COOKIE_SECURE") {
				t.Fatalf("Load() error = %v, want AUTH_COOKIE_SECURE error", err)
			}
		})
	}
}
