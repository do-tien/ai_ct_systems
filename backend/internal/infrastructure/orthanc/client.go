// Package orthanc provides the Orthanc integration adapter.
package orthanc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxSystemBodyBytes = 64 * 1024

// Client checks Orthanc availability through its REST API.
type Client struct {
	httpClient *http.Client
	baseURL    string
	user       string
	password   string
}

// NewClient builds a client from injected dependencies. The base URL is
// normalized once so callers may pass a trailing slash.
func NewClient(httpClient *http.Client, baseURL string, user string, password string) *Client {
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		user:       user,
		password:   password,
	}
}

// Check reports whether Orthanc answers GET /system with HTTP 200 and a JSON
// object carrying a non-empty Version. The returned errors describe the failure
// for logs; they are never sent to the browser.
func (c *Client) Check(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/system", nil)
	if err != nil {
		return fmt.Errorf("build orthanc request: %w", err)
	}
	request.SetBasicAuth(c.user, c.password)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call orthanc: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("orthanc returned status %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxSystemBodyBytes))
	if err != nil {
		return fmt.Errorf("read orthanc response: %w", err)
	}

	var system struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(body, &system); err != nil {
		return fmt.Errorf("decode orthanc response: %w", err)
	}
	if system.Version == "" {
		return fmt.Errorf("orthanc response has an empty version")
	}
	return nil
}
