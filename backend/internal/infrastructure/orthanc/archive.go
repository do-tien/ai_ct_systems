package orthanc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxArchiveBytes bounds one compressed DICOM series archive download. It is a
// POC resource ceiling, not a clinical suitability limit.
const maxArchiveBytes = 1 << 30 // 1 GiB

// ArchiveRequestTimeout bounds one archive download end to end. It is generous
// because a full series is far larger than a single image.
const ArchiveRequestTimeout = 2 * time.Minute

// NewArchiveHTTPClient builds the dedicated archive transport: redirects are
// never followed, so credentials cannot be replayed to another origin.
func NewArchiveHTTPClient() *http.Client {
	return &http.Client{
		Timeout: ArchiveRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// NewArchiveClient builds the archive downloader on the existing Client type,
// using a transport dedicated to series downloads rather than the short-lived
// health probe or the image proxy.
func NewArchiveClient(httpClient *http.Client, baseURL, user, password string) *Client {
	client := NewClient(httpClient, baseURL, user, password)
	client.archiveLimit = maxArchiveBytes
	return client
}

// Download streams GET /series/{id}/archive into dst. The series identifier is
// validated before it is interpolated into the URL. The response must be 200
// and within the compressed cap; the upstream body and URL are never logged.
func (c *Client) Download(ctx context.Context, seriesID string, dst io.Writer) error {
	if !validOrthancID(seriesID) {
		return errors.New("invalid orthanc series identifier")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/series/"+seriesID+"/archive", nil)
	if err != nil {
		return errors.New("cannot build orthanc archive request")
	}
	request.SetBasicAuth(c.user, c.password)

	response, err := c.do(ctx, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("orthanc archive returned status %d", response.StatusCode)
	}

	limit := c.archiveLimit
	if limit <= 0 {
		limit = maxArchiveBytes
	}
	written, err := io.Copy(dst, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return errors.New("orthanc archive stream failed")
	}
	if written > limit {
		return errors.New("orthanc archive exceeds the size limit")
	}
	return nil
}
