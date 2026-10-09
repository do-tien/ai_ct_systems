// Package config loads and validates the backend's runtime configuration from
// environment variables.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
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
}

// Load reads configuration from the environment and validates it. Errors name
// the offending variable but never include its value.
func Load() (Config, error) {
	cfg := Config{HTTPAddr: envOrDefault("HTTP_ADDR", defaultHTTPAddr)}

	var err error
	if cfg.PostgresHost, err = required("POSTGRES_HOST"); err != nil {
		return Config{}, err
	}

	portRaw, err := required("POSTGRES_PORT")
	if err != nil {
		return Config{}, err
	}
	port, err := strconv.Atoi(portRaw)
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("POSTGRES_PORT must be an integer between 1 and 65535")
	}
	cfg.PostgresPort = port

	if cfg.PostgresDB, err = required("POSTGRES_DB"); err != nil {
		return Config{}, err
	}
	if cfg.PostgresUser, err = required("POSTGRES_USER"); err != nil {
		return Config{}, err
	}
	if cfg.PostgresPassword, err = required("POSTGRES_PASSWORD"); err != nil {
		return Config{}, err
	}

	if cfg.OrthancURL, err = required("ORTHANC_URL"); err != nil {
		return Config{}, err
	}
	if err := validateOrthancURL(cfg.OrthancURL); err != nil {
		return Config{}, err
	}
	if cfg.OrthancUser, err = required("ORTHANC_USER"); err != nil {
		return Config{}, err
	}
	if err := validateCredential("ORTHANC_USER", cfg.OrthancUser); err != nil {
		return Config{}, err
	}
	if cfg.OrthancPassword, err = required("ORTHANC_PASSWORD"); err != nil {
		return Config{}, err
	}
	if err := validateCredential("ORTHANC_PASSWORD", cfg.OrthancPassword); err != nil {
		return Config{}, err
	}

	return cfg, nil
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
