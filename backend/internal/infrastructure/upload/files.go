// Package upload stages and safely extracts one uploaded DICOM batch inside a
// private workspace. It knows nothing about HTTP status codes, the database or
// Orthanc: it turns one streamed multipart request into local files plus a
// skipped-entry count, enforcing byte, file and entry budgets and rejecting
// unsafe archives. The caller owns the returned Workspace and must call
// Cleanup on it. DICOM validity itself is decided by Orthanc, not here.
package upload

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path"
	"strings"
)

// Limits bound one upload request. Every value is enforced while streaming:
// bytes are counted as they are actually read, never taken from an archive
// header or a Content-Length.
type Limits struct {
	// MaxTotalBytes caps the expanded bytes of every staged file.
	MaxTotalBytes int64
	// MaxFileBytes caps one staged DICOM file.
	MaxFileBytes int64
	// MaxFiles caps the number of staged DICOM files.
	MaxFiles int
	// MaxEntries caps archive entries (directories and skipped sidecars too).
	MaxEntries int
}

// DefaultLimits returns the documented upload budgets.
func DefaultLimits() Limits {
	return Limits{
		MaxTotalBytes: 4 << 30,  // 4 GiB expanded
		MaxFileBytes:  64 << 20, // 64 MiB per DICOM file
		MaxFiles:      10_000,
		MaxEntries:    20_000,
	}
}

// Sentinel errors. Each names a distinct intake failure so the HTTP layer can
// map it to a status without inspecting error text.
var (
	// ErrTooLarge reports a per-file or total byte budget overflow.
	ErrTooLarge = errors.New("upload exceeds a size limit")
	// ErrTooManyFiles reports more staged DICOM files than allowed.
	ErrTooManyFiles = errors.New("upload contains too many files")
	// ErrTooManyEntries reports more archive entries than allowed.
	ErrTooManyEntries = errors.New("upload contains too many entries")
	// ErrUnsafeEntry reports an absolute, traversal, link or special entry.
	ErrUnsafeEntry = errors.New("upload contains an unsafe entry")
	// ErrMalformedArchive reports a truncated or undecodable archive.
	ErrMalformedArchive = errors.New("upload archive is malformed or truncated")
	// ErrNestedArchive reports an archive inside an archive.
	ErrNestedArchive = errors.New("nested archives are not supported")
	// ErrUnsupportedFormat reports an encrypted entry or an unusable container.
	ErrUnsupportedFormat = errors.New("unsupported upload format")
	// ErrUnknownField reports a multipart field other than "files".
	ErrUnknownField = errors.New("upload contains an unexpected field")
	// ErrCanceled reports a canceled request.
	ErrCanceled = errors.New("upload canceled")
)

// StagedFile is one file written into the workspace. Path is a server-generated
// local path, never a client filename.
type StagedFile struct {
	Path string
	Size int64
}

// Workspace is the private staging directory for one upload. It is removed
// whole by Cleanup.
type Workspace struct {
	dir     string
	files   []StagedFile
	skipped int
}

// Files returns the staged DICOM files in arrival order.
func (w *Workspace) Files() []StagedFile { return append([]StagedFile(nil), w.files...) }

// SkippedFiles returns how many index/sidecar entries were ignored.
func (w *Workspace) SkippedFiles() int { return w.skipped }

// Cleanup removes the whole staging directory.
func (w *Workspace) Cleanup() error {
	if w == nil || w.dir == "" {
		return nil
	}
	return os.RemoveAll(w.dir)
}

// Stage streams every part of one multipart request into a fresh workspace
// created under root. Archive parts are extracted safely; plain parts are
// staged as-is (DICOM has no mandatory extension, so the file is not judged
// here). On any error the workspace is removed and nil is returned.
func Stage(ctx context.Context, root string, reader *multipart.Reader, limits Limits) (*Workspace, error) {
	if reader == nil {
		return nil, errors.New("upload: nil multipart reader")
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if limits.MaxFiles <= 0 {
		limits.MaxFiles = DefaultLimits().MaxFiles
	}
	if limits.MaxEntries <= 0 {
		limits.MaxEntries = DefaultLimits().MaxEntries
	}
	dir, err := os.MkdirTemp(root, "upload-")
	if err != nil {
		return nil, fmt.Errorf("upload: cannot create a workspace: %w", err)
	}
	ws := &Workspace{dir: dir}
	stager := &stager{ctx: ctx, limits: limits, ws: ws}
	if err := stager.run(reader); err != nil {
		_ = ws.Cleanup()
		return nil, err
	}
	return ws, nil
}

// stager carries the counters shared by every part and archive entry.
type stager struct {
	ctx     context.Context
	limits  Limits
	ws      *Workspace
	total   int64
	entries int
	files   int
}

func (s *stager) run(reader *multipart.Reader) error {
	for {
		if err := contextErr(s.ctx); err != nil {
			return err
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("upload: cannot read a multipart part: %w", err)
		}
		partErr := s.part(part, part.FormName(), part.FileName())
		_ = part.Close()
		if partErr != nil {
			return partErr
		}
	}
}

// part handles one multipart part: archives are extracted, safe sidecars are
// skipped and everything else is treated as one DICOM candidate.
func (s *stager) part(src io.Reader, field, filename string) error {
	if field != "files" {
		return ErrUnknownField
	}
	if err := s.countEntry(); err != nil {
		return err
	}
	buffered := bufio.NewReaderSize(src, 4096)
	head, err := buffered.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("upload: cannot read a multipart part: %w", err)
	}
	switch detectArchive(head) {
	case kindZip:
		return s.extractPart(buffered, kindZip)
	case kindGzip:
		return s.extractPart(buffered, kindGzip)
	case kindTar:
		return s.extractPart(buffered, kindTar)
	default:
		if isSkippedName(filename) {
			if err := s.discard(buffered); err != nil {
				return err
			}
			s.ws.skipped++
			return nil
		}
		return s.stageReader(buffered, s.limits.MaxFileBytes)
	}
}

// extractPart spills one archive into a workspace-local temp file and extracts
// it. The container bytes do not count against the expanded-byte budget; only
// the extracted content does.
func (s *stager) extractPart(src io.Reader, kind archiveKind) error {
	file, err := os.CreateTemp(s.ws.dir, "archive-")
	if err != nil {
		return fmt.Errorf("upload: cannot stage an archive: %w", err)
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := io.Copy(file, src); err != nil {
		_ = file.Close()
		return fmt.Errorf("upload: cannot stage an archive: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("upload: cannot stage an archive: %w", err)
	}
	if err := contextErr(s.ctx); err != nil {
		return err
	}
	handle, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("upload: cannot read an archive: %w", err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("upload: cannot read an archive: %w", err)
	}
	switch kind {
	case kindZip:
		return s.extractZip(handle, info.Size())
	case kindTar:
		return s.extractTar(handle, 0)
	case kindGzip:
		return s.extractTar(handle, gzipCompressed)
	}
	return ErrUnsupportedFormat
}

// stageReader writes one non-skipped file into the workspace. It rejects a file
// that exceeds the per-file or remaining total budget.
func (s *stager) stageReader(src io.Reader, maxBytes int64) error {
	if err := s.countFile(); err != nil {
		return err
	}
	file, err := os.CreateTemp(s.ws.dir, "data-")
	if err != nil {
		return fmt.Errorf("upload: cannot stage a file: %w", err)
	}
	name := file.Name()
	written, copyErr := s.copyLimited(file, src, maxBytes)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(name)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("upload: cannot stage a file: %w", closeErr)
	}
	s.ws.files = append(s.ws.files, StagedFile{Path: name, Size: written})
	return nil
}

// discard reads a skipped entry to EOF so it still counts against the expanded
// byte budget: a sidecar must not be a way around the quota.
func (s *stager) discard(src io.Reader) error {
	_, err := s.copyLimited(io.Discard, src, s.remaining())
	return err
}

// copyLimited copies at most maxBytes (bounded further by the remaining total)
// and reports an overflow when the source has more.
func (s *stager) copyLimited(dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	limit := maxBytes
	if remaining := s.remaining(); limit <= 0 || limit > remaining {
		limit = remaining
	}
	if limit <= 0 {
		return 0, ErrTooLarge
	}
	written, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return written, fmt.Errorf("upload: cannot read upload data: %w", err)
	}
	if written > limit {
		return written, ErrTooLarge
	}
	s.total += written
	return written, nil
}

func (s *stager) remaining() int64 { return s.limits.MaxTotalBytes - s.total }

func (s *stager) countEntry() error {
	s.entries++
	if s.entries > s.limits.MaxEntries {
		return ErrTooManyEntries
	}
	return nil
}

func (s *stager) countFile() error {
	s.files++
	if s.files > s.limits.MaxFiles {
		return ErrTooManyFiles
	}
	return nil
}

// archiveKind identifies a container without trusting its name.
type archiveKind int

const (
	kindPlain archiveKind = iota
	kindZip
	kindTar
	kindGzip
)

// detectArchive inspects the first bytes of a stream: ZIP and gzip magic, or a
// plausible tar header. A plain DICOM file matches none of them.
func detectArchive(head []byte) archiveKind {
	if len(head) >= 4 && head[0] == 'P' && head[1] == 'K' &&
		(head[2] == 3 && head[3] == 4 || head[2] == 5 && head[3] == 6 || head[2] == 7 && head[3] == 8) {
		return kindZip
	}
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return kindGzip
	}
	if len(head) >= 512 && string(head[257:262]) == "ustar" {
		return kindTar
	}
	return kindPlain
}

// safeEntryName validates one archive entry name and returns its cleaned
// slash-separated relative path. Absolute, drive, UNC and traversal names are
// rejected (backslashes are treated as separators before the check).
func safeEntryName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", ErrUnsafeEntry
	}
	normalized := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(normalized, "/") || strings.HasPrefix(normalized, "//") {
		return "", ErrUnsafeEntry
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return "", ErrUnsafeEntry
	}
	segments := strings.Split(normalized, "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".":
			continue
		case "..":
			return "", ErrUnsafeEntry
		default:
			cleaned = append(cleaned, segment)
		}
	}
	if len(cleaned) == 0 {
		return "", ErrUnsafeEntry
	}
	return strings.Join(cleaned, "/"), nil
}

// isSkippedPath reports whether an archive member is an index or sidecar file
// that must be ignored rather than imported.
func isSkippedPath(rel string) bool {
	if strings.HasPrefix(strings.ToLower(rel), "__macosx/") {
		return true
	}
	return isSkippedName(path.Base(rel))
}

// isSkippedName reports whether a filename is a known non-image entry.
func isSkippedName(name string) bool {
	base := path.Base(strings.ReplaceAll(name, "\\", "/"))
	switch strings.ToLower(base) {
	case "dicomdir", ".ds_store", "thumbs.db":
		return true
	default:
		return false
	}
}

func contextErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return ErrCanceled
	}
	return nil
}
