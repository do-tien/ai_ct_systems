package upload

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"io"
	"os"
)

// gzipCompressed tells extractTar to decompress the container first.
const gzipCompressed = 1

// extractZip extracts a ZIP archive, rejecting encrypted entries, links,
// unsafe names and nested archives. Skipped sidecars are still drained so they
// count against the expanded-byte budget.
func (s *stager) extractZip(source *os.File, size int64) error {
	reader, err := zip.NewReader(source, size)
	if err != nil {
		return ErrMalformedArchive
	}
	for _, entry := range reader.File {
		if err := contextErr(s.ctx); err != nil {
			return err
		}
		if err := s.countEntry(); err != nil {
			return err
		}
		rel, err := safeEntryName(entry.Name)
		if err != nil {
			return err
		}
		if entry.Flags&0x1 != 0 {
			return ErrUnsupportedFormat
		}
		info := entry.FileInfo()
		if info.IsDir() {
			continue
		}
		if !info.Mode().IsRegular() {
			return ErrUnsafeEntry
		}
		if err := s.extractMember(rel, func() (io.ReadCloser, error) { return entry.Open() }); err != nil {
			return err
		}
	}
	return nil
}

// extractTar extracts a TAR (optionally gzip-compressed) archive with the same
// entry rules as ZIP. Anything that is not a regular file or a directory is
// rejected.
func (s *stager) extractTar(source *os.File, mode int) error {
	var reader io.Reader = source
	if mode == gzipCompressed {
		gz, err := gzip.NewReader(source)
		if err != nil {
			return ErrMalformedArchive
		}
		defer gz.Close()
		reader = gz
	}
	archive := tar.NewReader(reader)
	for {
		if err := contextErr(s.ctx); err != nil {
			return err
		}
		header, err := archive.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return ErrMalformedArchive
		}
		if err := s.countEntry(); err != nil {
			return err
		}
		rel, err := safeEntryName(header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return ErrUnsafeEntry
		}
		if err := s.extractMember(rel, func() (io.ReadCloser, error) { return io.NopCloser(archive), nil }); err != nil {
			return err
		}
	}
}

// extractMember applies the skip rule and the nested-archive check to one
// archive member, then stages it. The opener is only called for entries that
// survive the name, link, encryption and skip checks.
func (s *stager) extractMember(rel string, open func() (io.ReadCloser, error)) error {
	if isSkippedPath(rel) {
		reader, err := open()
		if err != nil {
			return ErrMalformedArchive
		}
		discardErr := s.discard(reader)
		_ = reader.Close()
		if discardErr != nil {
			return discardErr
		}
		s.ws.skipped++
		return nil
	}
	reader, err := open()
	if err != nil {
		return ErrMalformedArchive
	}
	defer reader.Close()
	buffered := bufio.NewReaderSize(reader, 4096)
	head, err := buffered.Peek(512)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return ErrMalformedArchive
	}
	if detectArchive(head) != kindPlain {
		return ErrNestedArchive
	}
	return s.stageReader(buffered, s.limits.MaxFileBytes)
}
