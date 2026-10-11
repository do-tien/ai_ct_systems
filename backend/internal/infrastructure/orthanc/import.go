package orthanc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
)

const (
	// maxUploadResponseBytes bounds the POST /instances reply.
	maxUploadResponseBytes = 1 << 20
	// maxTagResponseBytes bounds one /instances/{id}/simplified-tags reply.
	maxTagResponseBytes = 1 << 20
	// maxInventoryResponseBytes bounds a /series/{id} reply.
	maxInventoryResponseBytes = 4 << 20
	// ImportRequestTimeout bounds each importer HTTP request end to end.
	ImportRequestTimeout = 30 * time.Second
)

// orthancIDPattern matches Orthanc's 40-hex-digit identifier rendered as five
// groups of eight hex digits. Validated before interpolation into URL paths.
var orthancIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{8}){4}$`)

// NewImportHTTPClient builds the bounded HTTP client the importer uses: every
// request has a 30-second timeout and redirects are never followed, so
// credentials cannot be replayed to another origin.
func NewImportHTTPClient() *http.Client {
	return &http.Client{
		Timeout: ImportRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Upload streams one DICOM file to POST /instances. The caller keeps ownership
// of body: the wrapped reader absorbs the close that net/http performs on a
// ReadCloser request body, so the file is only closed by its opener.
func (c *Client) Upload(ctx context.Context, body io.Reader, size int64) (caseimport.UploadedInstance, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/instances", io.NopCloser(body))
	if err != nil {
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrDependency, "cannot build orthanc upload request")
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/dicom")
	request.SetBasicAuth(c.user, c.password)

	response, err := c.do(ctx, request)
	if err != nil {
		return caseimport.UploadedInstance{}, err
	}
	defer response.Body.Close()

	// The status is decided before the body is read so a large error page from
	// Orthanc stays a dependency failure instead of looking like bad input.
	if response.StatusCode != http.StatusOK {
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrDependency,
			fmt.Sprintf("orthanc upload rejected with status %d", response.StatusCode))
	}

	raw, err := readBounded(response.Body, maxUploadResponseBytes)
	if err != nil {
		return caseimport.UploadedInstance{}, readError(err, "orthanc upload response")
	}

	var payload struct {
		ID           string `json:"ID"`
		ParentSeries string `json:"ParentSeries"`
		ParentStudy  string `json:"ParentStudy"`
		Status       string `json:"Status"`
	}
	if err := decodeStrict(raw, &payload); err != nil {
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrInvalidInput, "orthanc upload reply is not valid JSON")
	}
	if payload.Status != "Success" && payload.Status != "AlreadyStored" {
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrInvalidInput, "orthanc upload reply has an unsupported status")
	}
	if !validOrthancID(payload.ID) || !validOrthancID(payload.ParentSeries) || !validOrthancID(payload.ParentStudy) {
		return caseimport.UploadedInstance{}, caseimport.Fail(caseimport.ErrInvalidInput, "orthanc upload reply lacks valid identifiers")
	}

	return caseimport.UploadedInstance{
		ID:            strings.ToLower(payload.ID),
		SeriesID:      strings.ToLower(payload.ParentSeries),
		StudyID:       strings.ToLower(payload.ParentStudy),
		AlreadyStored: payload.Status == "AlreadyStored",
	}, nil
}

// DescribeSeries reads the series inventory, its parent study and the
// simplified tags of every instance. Per-instance metadata is validated for
// presence, type and finiteness before fixed-size fields are built.
func (c *Client) DescribeSeries(ctx context.Context, seriesID string) (caseimport.SeriesMetadata, error) {
	if !validOrthancID(seriesID) {
		return caseimport.SeriesMetadata{}, caseimport.Fail(caseimport.ErrInvalidInput, "invalid orthanc series identifier")
	}

	inventory, parentStudy, seriesUID, err := c.seriesInventory(ctx, seriesID)
	if err != nil {
		return caseimport.SeriesMetadata{}, err
	}

	studyUID, err := c.studyInstanceUID(ctx, parentStudy)
	if err != nil {
		return caseimport.SeriesMetadata{}, err
	}

	metadata := caseimport.SeriesMetadata{
		SeriesID:          seriesID,
		StudyID:           parentStudy,
		StudyInstanceUID:  studyUID,
		SeriesInstanceUID: seriesUID,
		Instances:         make([]caseimport.InstanceMetadata, 0, len(inventory)),
	}
	for _, instanceID := range inventory {
		if !validOrthancID(instanceID) {
			return caseimport.SeriesMetadata{}, caseimport.Fail(caseimport.ErrInvalidInput, "series inventory contains an invalid identifier")
		}
		tags, err := c.instanceTags(ctx, instanceID)
		if err != nil {
			return caseimport.SeriesMetadata{}, err
		}
		instance, err := parseInstanceMetadata(instanceID, tags, seriesUID, studyUID)
		if err != nil {
			return caseimport.SeriesMetadata{}, err
		}
		metadata.Instances = append(metadata.Instances, instance)
	}
	return metadata, nil
}

// ListSeriesInstances returns the current instance inventory of a series. The
// import use case calls it immediately before persistence to detect a series
// that changed while its files were being uploaded.
func (c *Client) ListSeriesInstances(ctx context.Context, seriesID string) ([]string, error) {
	if !validOrthancID(seriesID) {
		return nil, caseimport.Fail(caseimport.ErrInvalidInput, "invalid orthanc series identifier")
	}
	inventory, _, _, err := c.seriesInventory(ctx, seriesID)
	if err != nil {
		return nil, err
	}
	for _, id := range inventory {
		if !validOrthancID(id) {
			return nil, caseimport.Fail(caseimport.ErrInvalidInput, "series inventory contains an invalid identifier")
		}
	}
	return inventory, nil
}

// seriesInventory reads GET /series/{id} and returns the raw instance order,
// the parent study identifier and the Series Instance UID. Extra Orthanc fields
// are not retained.
func (c *Client) seriesInventory(ctx context.Context, seriesID string) ([]string, string, string, error) {
	raw, err := c.getJSON(ctx, c.baseURL+"/series/"+seriesID, maxInventoryResponseBytes)
	if err != nil {
		return nil, "", "", err
	}
	var payload struct {
		ID            string `json:"ID"`
		ParentStudy   string `json:"ParentStudy"`
		Instances     []string
		MainDicomTags struct {
			SeriesInstanceUID string
		}
	}
	if err := decodeStrict(raw, &payload); err != nil {
		return nil, "", "", caseimport.Fail(caseimport.ErrInvalidInput, "orthanc series reply is not valid JSON")
	}
	if payload.ID != "" && !strings.EqualFold(payload.ID, seriesID) {
		return nil, "", "", caseimport.Fail(caseimport.ErrInvalidInput, "orthanc series reply identifies another series")
	}
	if !validOrthancID(payload.ParentStudy) {
		return nil, "", "", caseimport.Fail(caseimport.ErrInvalidInput, "orthanc series reply lacks a valid parent study")
	}
	if strings.TrimSpace(payload.MainDicomTags.SeriesInstanceUID) == "" {
		return nil, "", "", caseimport.Fail(caseimport.ErrInvalidInput, "orthanc series reply lacks a series instance UID")
	}
	return payload.Instances, strings.ToLower(payload.ParentStudy), payload.MainDicomTags.SeriesInstanceUID, nil
}

// studyInstanceUID reads GET /studies/{id} and returns the Study Instance UID.
func (c *Client) studyInstanceUID(ctx context.Context, studyID string) (string, error) {
	raw, err := c.getJSON(ctx, c.baseURL+"/studies/"+studyID, maxInventoryResponseBytes)
	if err != nil {
		return "", err
	}
	var payload struct {
		MainDicomTags struct {
			StudyInstanceUID string
		}
	}
	if err := decodeStrict(raw, &payload); err != nil {
		return "", caseimport.Fail(caseimport.ErrInvalidInput, "orthanc study reply is not valid JSON")
	}
	if strings.TrimSpace(payload.MainDicomTags.StudyInstanceUID) == "" {
		return "", caseimport.Fail(caseimport.ErrInvalidInput, "orthanc study reply lacks a study instance UID")
	}
	return payload.MainDicomTags.StudyInstanceUID, nil
}

// instanceTags reads GET /instances/{id}/simplified-tags.
func (c *Client) instanceTags(ctx context.Context, instanceID string) (map[string]json.RawMessage, error) {
	raw, err := c.getJSON(ctx, c.baseURL+"/instances/"+instanceID+"/simplified-tags", maxTagResponseBytes)
	if err != nil {
		return nil, err
	}
	var tags map[string]json.RawMessage
	if err := decodeStrict(raw, &tags); err != nil {
		return nil, caseimport.Fail(caseimport.ErrInvalidInput, "orthanc instance reply is not valid JSON")
	}
	return tags, nil
}

// getJSON performs one bounded, authenticated GET and returns the raw body.
func (c *Client) getJSON(ctx context.Context, url string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, caseimport.Fail(caseimport.ErrDependency, "cannot build orthanc request")
	}
	request.SetBasicAuth(c.user, c.password)

	response, err := c.do(ctx, request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	// Status first: a large error page from Orthanc must stay a dependency
	// failure rather than being reported as invalid input.
	if response.StatusCode != http.StatusOK {
		return nil, caseimport.Fail(caseimport.ErrDependency,
			fmt.Sprintf("orthanc metadata request rejected with status %d", response.StatusCode))
	}

	raw, err := readBounded(response.Body, limit)
	if err != nil {
		return nil, readError(err, "orthanc metadata response")
	}
	return raw, nil
}

// do issues the request and categorizes transport and redirect failures without
// leaking the underlying error, URL or credentials.
func (c *Client) do(ctx context.Context, request *http.Request) (*http.Response, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, caseimport.Fail(caseimport.ErrCanceled, "orthanc request canceled")
		}
		return nil, caseimport.Fail(caseimport.ErrDependency, "orthanc request failed")
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, caseimport.Fail(caseimport.ErrDependency, "orthanc redirect refused")
	}
	return response, nil
}

// errTooLarge marks a response that exceeded its byte cap.
var errTooLarge = errors.New("response exceeds byte cap")

// readBounded reads at most limit bytes and reports overflow without buffering
// the rest of the response.
func readBounded(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errTooLarge
	}
	return raw, nil
}

func readError(err error, label string) error {
	if errors.Is(err, errTooLarge) {
		return caseimport.Fail(caseimport.ErrInvalidInput, label+" exceeds the size limit")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return caseimport.Fail(caseimport.ErrCanceled, label+" was interrupted")
	}
	return caseimport.Fail(caseimport.ErrDependency, label+" could not be read")
}

// decodeStrict decodes exactly one JSON value and rejects trailing content.
func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

func validOrthancID(id string) bool {
	return orthancIDPattern.MatchString(id)
}

// parseInstanceMetadata builds one instance's fixed-size metadata from its
// simplified tags. Presence and finiteness are enforced here; positivity and
// consistency belong to the use case.
func parseInstanceMetadata(id string, tags map[string]json.RawMessage, seriesUID, studyUID string) (caseimport.InstanceMetadata, error) {
	fail := func() (caseimport.InstanceMetadata, error) {
		return caseimport.InstanceMetadata{}, caseimport.Fail(caseimport.ErrInvalidInput, "orthanc instance metadata is incomplete or malformed")
	}

	frameUID, err := requiredString(tags, "FrameOfReferenceUID")
	if err != nil {
		return fail()
	}
	sopUID, err := requiredString(tags, "SOPInstanceUID")
	if err != nil {
		return fail()
	}
	sopClass, err := requiredString(tags, "SOPClassUID")
	if err != nil {
		return fail()
	}
	modality, err := requiredString(tags, "Modality")
	if err != nil {
		return fail()
	}
	imageType, err := requiredStringList(tags, "ImageType")
	if err != nil {
		return fail()
	}
	rows, err := requiredInt(tags, "Rows")
	if err != nil {
		return fail()
	}
	columns, err := requiredInt(tags, "Columns")
	if err != nil {
		return fail()
	}
	spacing, err := requiredFloats(tags, "PixelSpacing", 2)
	if err != nil {
		return fail()
	}
	orientation, err := requiredFloats(tags, "ImageOrientationPatient", 6)
	if err != nil {
		return fail()
	}
	position, err := requiredFloats(tags, "ImagePositionPatient", 3)
	if err != nil {
		return fail()
	}
	thickness, err := requiredFloat(tags, "SliceThickness")
	if err != nil {
		return fail()
	}

	// Cross-check the parents when Orthanc reports them on the instance.
	if reported, err := optionalString(tags, "SeriesInstanceUID"); err == nil && reported != "" && reported != seriesUID {
		return fail()
	} else if err != nil {
		return fail()
	}
	if reported, err := optionalString(tags, "StudyInstanceUID"); err == nil && reported != "" && reported != studyUID {
		return fail()
	} else if err != nil {
		return fail()
	}

	instance := caseimport.InstanceMetadata{
		ID:                      strings.ToLower(id),
		FrameOfReferenceUID:     frameUID,
		SOPInstanceUID:          sopUID,
		SOPClassUID:             sopClass,
		Modality:                modality,
		ImageType:               imageType,
		Rows:                    rows,
		Columns:                 columns,
		PixelSpacing:            [2]float64{spacing[0], spacing[1]},
		ImageOrientationPatient: [6]float64{orientation[0], orientation[1], orientation[2], orientation[3], orientation[4], orientation[5]},
		ImagePositionPatient:    [3]float64{position[0], position[1], position[2]},
		SliceThickness:          thickness,
	}

	// NumberOfFrames is optional; only single-frame or absent is supported.
	if raw, ok := tags["NumberOfFrames"]; ok {
		value, err := parseIntScalar(raw)
		if err != nil {
			return fail()
		}
		instance.NumberOfFrames = value
	}

	if !finite(spacing[0], spacing[1], thickness, orientation[0], orientation[1], orientation[2], orientation[3],
		orientation[4], orientation[5], position[0], position[1], position[2]) {
		return fail()
	}
	return instance, nil
}

func finite(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func requiredString(tags map[string]json.RawMessage, name string) (string, error) {
	raw, ok := tags[name]
	if !ok {
		return "", errors.New("missing tag")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("wrong tag type")
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("empty tag value")
	}
	return strings.TrimSpace(value), nil
}

func optionalString(tags map[string]json.RawMessage, name string) (string, error) {
	raw, ok := tags[name]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("wrong tag type")
	}
	return strings.TrimSpace(value), nil
}

func requiredStringList(tags map[string]json.RawMessage, name string) ([]string, error) {
	raw, ok := tags[name]
	if !ok {
		return nil, errors.New("missing tag")
	}
	values, err := parseStringList(raw)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, errors.New("empty tag value")
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, errors.New("empty tag component")
		}
	}
	return values, nil
}

func parseStringList(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return strings.Split(single, "\\"), nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, errors.New("wrong tag type")
}

func requiredInt(tags map[string]json.RawMessage, name string) (int, error) {
	raw, ok := tags[name]
	if !ok {
		return 0, errors.New("missing tag")
	}
	return parseIntScalar(raw)
}

func parseIntScalar(raw json.RawMessage) (int, error) {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strconv.Atoi(strings.TrimSpace(text))
	}
	return 0, errors.New("wrong tag type")
}

func requiredFloat(tags map[string]json.RawMessage, name string) (float64, error) {
	raw, ok := tags[name]
	if !ok {
		return 0, errors.New("missing tag")
	}
	return parseNumericScalar(raw)
}

func requiredFloats(tags map[string]json.RawMessage, name string, want int) ([]float64, error) {
	raw, ok := tags[name]
	if !ok {
		return nil, errors.New("missing tag")
	}
	values, err := parseNumericArray(raw)
	if err != nil {
		return nil, err
	}
	if len(values) != want {
		return nil, errors.New("wrong tag length")
	}
	return values, nil
}

func parseNumericArray(raw json.RawMessage) ([]float64, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parts := strings.Split(text, "\\")
		values := make([]float64, 0, len(parts))
		for _, part := range parts {
			value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		values := make([]float64, 0, len(list))
		for _, element := range list {
			value, err := parseNumericScalar(element)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}
	value, err := parseNumericScalar(raw)
	if err != nil {
		return nil, err
	}
	return []float64{value}, nil
}

func parseNumericScalar(raw json.RawMessage) (float64, error) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strconv.ParseFloat(strings.TrimSpace(text), 64)
	}
	return 0, errors.New("wrong tag type")
}
