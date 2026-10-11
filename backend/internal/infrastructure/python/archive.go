// Package python implements the LC1-12 local process adapter: it owns the
// per-job workspace, extracts the downloaded DICOM series safely, runs the
// inference CLI as an argv child process with a bounded budget and returns only
// a fresh result file or a fixed failure category.
package python

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// POC resource ceilings for one extracted archive. They bound disk and CPU use
// on the POC host; they are not clinical suitability limits and do not describe
// what a real series may legally contain.
const (
	defaultArchiveTotalBytes int64 = 4 << 30   // 4 GiB expanded
	defaultArchiveFileBytes  int64 = 512 << 20 // 512 MiB per file
	defaultArchiveEntries          = 10000
)

// errArchiveInvalid is the single fixed category for an unsafe or malformed
// archive. Callers map it to aijob.CodeArchiveInvalid.
var errArchiveInvalid = errors.New("archive is invalid or unsafe")

// archiveLimits carries the streaming extraction ceilings so tests can use
// reduced values without producing a real decompression bomb.
type archiveLimits struct {
	totalBytes int64
	fileBytes  int64
	entries    int
}

func defaultArchiveLimits() archiveLimits {
	return archiveLimits{
		totalBytes: defaultArchiveTotalBytes,
		fileBytes:  defaultArchiveFileBytes,
		entries:    defaultArchiveEntries,
	}
}

// extractArchive extracts a DICOM series ZIP into destDir, flattening every
// entry into destDir: real Orthanc archives group instances under
// patient/study/series folders, but the inference CLI discovers the series with
// a non-recursive directory listing, so the series directory must be flat.
//
// Every entry name is still validated so nothing can escape destDir; files are
// created exclusively; duplicate base names, file/directory collisions and the
// per-file, total and entry-count ceilings are rejected.
func extractArchive(ctx context.Context, archivePath, destDir string, limits archiveLimits) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return errArchiveInvalid
	}
	defer reader.Close()

	if len(reader.File) > limits.entries {
		return errArchiveInvalid
	}

	seen := make(map[string]int8, len(reader.File))
	var total int64
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, isDir, err := safeArchiveEntry(file.Name)
		if err != nil {
			return err
		}
		base := filepath.Base(filepath.FromSlash(rel))
		if err := registerArchiveEntry(seen, base, !isDir); err != nil {
			return err
		}
		target := filepath.Join(destDir, base)
		if !withinDir(destDir, target) {
			return errArchiveInvalid
		}
		if isDir {
			continue
		}
		if err := extractArchiveFile(ctx, file, target, limits, &total); err != nil {
			return err
		}
	}
	return nil
}

func extractArchiveFile(ctx context.Context, file *zip.File, target string, limits archiveLimits, total *int64) error {
	if file.Flags&0x1 != 0 { // encrypted entry
		return errArchiveInvalid
	}
	mode := file.FileInfo().Mode()
	if mode&os.ModeSymlink != 0 || !mode.IsRegular() {
		return errArchiveInvalid
	}
	if int64(file.UncompressedSize64) > limits.fileBytes {
		return errArchiveInvalid
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return errArchiveInvalid
	}

	source, err := file.Open()
	if err != nil {
		return errArchiveInvalid
	}
	defer source.Close()

	destination, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errArchiveInvalid
	}

	written, copyErr := copyWithContext(ctx, destination, io.LimitReader(source, limits.fileBytes+1))
	if closeErr := destination.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(target)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errArchiveInvalid
	}
	if written > limits.fileBytes {
		_ = os.Remove(target)
		return errArchiveInvalid
	}

	*total += written
	if *total > limits.totalBytes {
		_ = os.Remove(target)
		return errArchiveInvalid
	}
	return nil
}

// copyWithContext streams in chunks so a canceled job budget stops a large
// expansion instead of decompressing it to completion.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := src.Read(buffer)
		if read > 0 {
			written, writeErr := dst.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

// safeArchiveEntry validates one ZIP entry name and returns its slash-separated
// relative path. It rejects anything that could escape the destination or is
// ambiguous on Windows: absolute or UNC paths, drive letters, backslashes,
// control characters, "."/".." components, ADS colons, reserved device names
// and trailing spaces or dots.
func safeArchiveEntry(name string) (string, bool, error) {
	if name == "" || strings.ContainsRune(name, '\\') {
		return "", false, errArchiveInvalid
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", false, errArchiveInvalid
		}
	}
	isDir := strings.HasSuffix(name, "/")
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || hasDrivePrefix(trimmed) {
		return "", false, errArchiveInvalid
	}

	parts := strings.Split(trimmed, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", false, errArchiveInvalid
		}
		if strings.ContainsRune(part, ':') {
			return "", false, errArchiveInvalid
		}
		if part != strings.TrimRight(part, " .") {
			return "", false, errArchiveInvalid
		}
		if isReservedDeviceName(part) {
			return "", false, errArchiveInvalid
		}
	}
	return strings.Join(parts, "/"), isDir, nil
}

func hasDrivePrefix(name string) bool {
	if len(name) < 2 || name[1] != ':' {
		return false
	}
	c := name[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isReservedDeviceName(part string) bool {
	base := part
	if index := strings.IndexByte(base, '.'); index >= 0 {
		base = base[:index]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 {
		upper := strings.ToUpper(base)
		if (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9' {
			return true
		}
	}
	return false
}

// registerArchiveEntry records a case-insensitive base name and rejects
// duplicate targets and file/directory collisions.
func registerArchiveEntry(seen map[string]int8, base string, isFile bool) error {
	key := strings.ToLower(base)
	if _, ok := seen[key]; ok {
		return errArchiveInvalid
	}
	if isFile {
		seen[key] = 1
	} else {
		seen[key] = 0
	}
	return nil
}

// withinDir reports whether target stays beneath dir after cleaning.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
