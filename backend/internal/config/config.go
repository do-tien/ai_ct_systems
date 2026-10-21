// Package config loads and validates the backend's runtime configuration from
// environment variables.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nodule-reader/backend/internal/infrastructure/postgres"
)

const defaultHTTPAddr = ":8080"

// Config holds every setting the backend needs at startup. All fields except
// HTTPAddr are required so a misconfigured deployment fails loudly.
type Config struct {
	HTTPAddr         string
	PostgresHost     string
	PostgresPort     int
	PostgresDB       string
	PostgresUser     string
	PostgresPassword string
	OrthancURL       string
	OrthancUser      string
	OrthancPassword  string
	AIPython         string
	AIBundleDir      string
	AIJobDir         string
	AITimeout        time.Duration
	AIDevice         string
	UploadDir        string
}

// LoadPostgres reads only the POSTGRES_* variables needed to reach the
// database. It is independent of Orthanc so tools such as the migration
// command can run without an Orthanc configuration.
func LoadPostgres() (postgres.Settings, error) {
	host, err := required("POSTGRES_HOST")
	if err != nil {
		return postgres.Settings{}, err
	}

	portRaw, err := required("POSTGRES_PORT")
	if err != nil {
		return postgres.Settings{}, err
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return postgres.Settings{}, fmt.Errorf("POSTGRES_PORT must be an integer between 1 and 65535")
	}

	database, err := required("POSTGRES_DB")
	if err != nil {
		return postgres.Settings{}, err
	}
	user, err := required("POSTGRES_USER")
	if err != nil {
		return postgres.Settings{}, err
	}
	password, err := required("POSTGRES_PASSWORD")
	if err != nil {
		return postgres.Settings{}, err
	}

	return postgres.Settings{
		Host:     host,
		Port:     port,
		Database: database,
		User:     user,
		Password: password,
	}, nil
}

// OrthancSettings holds the validated Orthanc connection settings.
type OrthancSettings struct {
	URL      string
	User     string
	Password string
}

// LoadOrthanc reads and validates the ORTHANC_* variables. It is independent of
// the AI worker configuration so commands such as the importer can run without
// a configured inference environment.
func LoadOrthanc() (OrthancSettings, error) {
	settings := OrthancSettings{}
	var err error
	if settings.URL, err = required("ORTHANC_URL"); err != nil {
		return OrthancSettings{}, err
	}
	if err := validateOrthancURL(settings.URL); err != nil {
		return OrthancSettings{}, err
	}
	if settings.User, err = required("ORTHANC_USER"); err != nil {
		return OrthancSettings{}, err
	}
	if err := validateCredential("ORTHANC_USER", settings.User); err != nil {
		return OrthancSettings{}, err
	}
	if settings.Password, err = required("ORTHANC_PASSWORD"); err != nil {
		return OrthancSettings{}, err
	}
	if err := validateCredential("ORTHANC_PASSWORD", settings.Password); err != nil {
		return OrthancSettings{}, err
	}
	return settings, nil
}

// Load reads configuration from the environment and validates it. Errors name
// the offending variable but never include its value.
func Load() (Config, error) {
	cfg := Config{HTTPAddr: envOrDefault("HTTP_ADDR", defaultHTTPAddr)}

	settings, err := LoadPostgres()
	if err != nil {
		return Config{}, err
	}
	cfg.PostgresHost = settings.Host
	cfg.PostgresPort = settings.Port
	cfg.PostgresDB = settings.Database
	cfg.PostgresUser = settings.User
	cfg.PostgresPassword = settings.Password

	orthancSettings, err := LoadOrthanc()
	if err != nil {
		return Config{}, err
	}
	cfg.OrthancURL = orthancSettings.URL
	cfg.OrthancUser = orthancSettings.User
	cfg.OrthancPassword = orthancSettings.Password

	if cfg.AIPython, err = required("AI_PYTHON"); err != nil {
		return Config{}, err
	}
	if err := validateExecutableFile("AI_PYTHON", cfg.AIPython); err != nil {
		return Config{}, err
	}
	if cfg.AIBundleDir, err = required("AI_BUNDLE_DIR"); err != nil {
		return Config{}, err
	}
	if err := validateExistingDirectory("AI_BUNDLE_DIR", cfg.AIBundleDir); err != nil {
		return Config{}, err
	}
	if cfg.AIJobDir, err = required("AI_JOB_DIR"); err != nil {
		return Config{}, err
	}
	if err := prepareJobDir(cfg.AIJobDir, cfg.AIBundleDir); err != nil {
		return Config{}, err
	}
	if cfg.AITimeout, err = parseAITimeout(); err != nil {
		return Config{}, err
	}
	if cfg.AIDevice, err = parseAIDevice(); err != nil {
		return Config{}, err
	}

	if cfg.UploadDir, err = required("UPLOAD_DIR"); err != nil {
		return Config{}, err
	}
	if err := prepareUploadDir(cfg.UploadDir, cfg.AIBundleDir, cfg.AIJobDir); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validateExecutableFile requires an absolute path to an existing regular file,
// so no shell or PATH lookup ever happens.
func validateExecutableFile(name, value string) error {
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path", name)
	}
	info, err := os.Stat(value)
	if err != nil {
		return fmt.Errorf("%s must point to an existing file", name)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must point to a regular file", name)
	}
	return nil
}

// validateExistingDirectory requires an absolute path to an existing directory.
func validateExistingDirectory(name, value string) error {
	if !filepath.IsAbs(value) {
		return fmt.Errorf("%s must be an absolute path", name)
	}
	info, err := os.Stat(value)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s must point to an existing directory", name)
	}
	return nil
}

// prepareJobDir creates the private job root when absent, confirms it is a
// writable directory and rejects any overlap with the read-only bundle.
func prepareJobDir(jobDir, bundleDir string) error {
	if !filepath.IsAbs(jobDir) {
		return fmt.Errorf("AI_JOB_DIR must be an absolute path")
	}
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		return fmt.Errorf("AI_JOB_DIR must be a creatable directory")
	}
	info, err := os.Stat(jobDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("AI_JOB_DIR must be a directory")
	}
	if pathsOverlap(jobDir, bundleDir) {
		return fmt.Errorf("AI_JOB_DIR must not overlap AI_BUNDLE_DIR")
	}
	probe, err := os.CreateTemp(jobDir, ".ai-job-probe-")
	if err != nil {
		return fmt.Errorf("AI_JOB_DIR must be writable")
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		return fmt.Errorf("AI_JOB_DIR must be writable")
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("AI_JOB_DIR must be writable")
	}
	return nil
}

// prepareUploadDir creates the private upload root when absent, confirms it is
// a writable directory and rejects any overlap with the AI bundle or job roots
// so staged DICOM can never land inside a read-only or inference directory.
func prepareUploadDir(uploadDir string, forbidden ...string) error {
	if !filepath.IsAbs(uploadDir) {
		return fmt.Errorf("UPLOAD_DIR must be an absolute path")
	}
	if err := os.MkdirAll(uploadDir, 0o700); err != nil {
		return fmt.Errorf("UPLOAD_DIR must be a creatable directory")
	}
	info, err := os.Stat(uploadDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("UPLOAD_DIR must be a directory")
	}
	for _, other := range forbidden {
		if other != "" && pathsOverlap(uploadDir, other) {
			return fmt.Errorf("UPLOAD_DIR must not overlap the AI bundle or job directory")
		}
	}
	probe, err := os.CreateTemp(uploadDir, ".upload-probe-")
	if err != nil {
		return fmt.Errorf("UPLOAD_DIR must be writable")
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		return fmt.Errorf("UPLOAD_DIR must be writable")
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("UPLOAD_DIR must be writable")
	}
	return nil
}

// pathsOverlap reports whether one cleaned absolute path contains the other.
func pathsOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	cleanA := filepath.Clean(absA)
	cleanB := filepath.Clean(absB)
	rel, err := filepath.Rel(cleanA, cleanB)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return true
	}
	rel, err = filepath.Rel(cleanB, cleanA)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func parseAITimeout() (time.Duration, error) {
	raw := envOrDefault("AI_TIMEOUT", "15m")
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("AI_TIMEOUT must be a positive Go duration")
	}
	return duration, nil
}

var aiDevicePattern = regexp.MustCompile(`^(cpu|cuda|cuda:[0-9]+)$`)

func parseAIDevice() (string, error) {
	value := envOrDefault("AI_DEVICE", "cuda")
	if !aiDevicePattern.MatchString(value) {
		return "", fmt.Errorf("AI_DEVICE must be cpu, cuda, or cuda:N")
	}
	return value, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func required(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("missing required environment variable %s", name)
	}
	return value, nil
}

func validateOrthancURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("ORTHANC_URL is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("ORTHANC_URL must use http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("ORTHANC_URL must include a host")
	}
	if parsed.User != nil {
		return fmt.Errorf("ORTHANC_URL must not embed credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("ORTHANC_URL must not include a query or fragment")
	}
	return nil
}

func validateCredential(name, value string) error {
	for _, r := range value {
		valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !valid {
			return fmt.Errorf("%s must contain only letters, numbers, hyphen, or underscore", name)
		}
	}
	return nil
}
