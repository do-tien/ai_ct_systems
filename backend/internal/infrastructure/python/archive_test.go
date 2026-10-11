package python

import (
	"archive/zip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type zipEntry struct {
	Name string
	Data []byte
	Mode fs.FileMode
	// raw, when set, writes the header with these declared sizes via CreateRaw
	// so a dishonest declared size can be tested without real disk use.
	raw           bool
	declaredSize  uint64
	compressedLen uint64
}

func writeArchive(t *testing.T, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	writer := zip.NewWriter(file)
	for _, entry := range entries {
		if entry.raw {
			header := &zip.FileHeader{
				Name:               entry.Name,
				Method:             zip.Store,
				UncompressedSize64: entry.declaredSize,
				CompressedSize64:   entry.compressedLen,
			}
			raw, err := writer.CreateRaw(header)
			if err != nil {
				t.Fatalf("CreateRaw(%s): %v", entry.Name, err)
			}
			if _, err := raw.Write(entry.Data); err != nil {
				t.Fatalf("write raw %s: %v", entry.Name, err)
			}
			continue
		}
		header := &zip.FileHeader{Name: entry.Name}
		if entry.Mode != 0 {
			header.SetMode(entry.Mode)
		}
		target, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("CreateHeader(%s): %v", entry.Name, err)
		}
		if _, err := target.Write(entry.Data); err != nil {
			t.Fatalf("write %s: %v", entry.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return path
}

func testLimits() archiveLimits {
	return archiveLimits{totalBytes: 1 << 20, fileBytes: 1 << 18, entries: 100}
}

func TestArchiveExtractFlattensNestedEntries(t *testing.T) {
	// Real Orthanc archives are nested, but the inference CLI lists the series
	// directory without recursion, so every instance must land directly in it.
	archive := writeArchive(t, []zipEntry{
		{Name: "a.dcm", Data: []byte("one")},
		{Name: "sub/b.dcm", Data: []byte("two")},
		{Name: "sub/deep/c.dcm", Data: []byte("three")},
	})
	dest := filepath.Join(t.TempDir(), "dicom")

	if err := extractArchive(context.Background(), archive, dest, testLimits()); err != nil {
		t.Fatalf("extractArchive() error = %v", err)
	}
	for name, want := range map[string]string{"a.dcm": "one", "b.dcm": "two", "c.dcm": "three"} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || string(got) != want {
			t.Fatalf("read %s = %q, %v; want %q", name, got, err, want)
		}
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("extraction left a nested directory %q", entry.Name())
		}
	}
}

func TestArchiveExtractRejectsUnsafeNames(t *testing.T) {
	names := []string{
		"../escape.dcm",
		"a/../../escape.dcm",
		"/absolute.dcm",
		"C:/drive.dcm",
		"C:\\drive.dcm",
		"\\\\server\\share\\x.dcm",
		"back\\slash.dcm",
		"nul\x00byte.dcm",
		"tab\u0009char.dcm",
		".",
		"..",
		"a/./b.dcm",
		"colon:stream.dcm",
		"CON",
		"con.dcm",
		"NUL.dcm",
		"COM1.dcm",
		"trailing.dcm ",
		"trailing.dcm.",
		"dir /file.dcm",
		"a//b.dcm",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			archive := writeArchive(t, []zipEntry{{Name: name, Data: []byte("x")}})
			dest := filepath.Join(t.TempDir(), "dicom")
			if err := extractArchive(context.Background(), archive, dest, testLimits()); err == nil {
				t.Fatalf("extractArchive() accepted unsafe name %q", name)
			}
		})
	}
}

func TestArchiveExtractRejectsSymlink(t *testing.T) {
	archive := writeArchive(t, []zipEntry{{Name: "link", Data: []byte("../target"), Mode: os.ModeSymlink | 0o777}})
	dest := filepath.Join(t.TempDir(), "dicom")
	if err := extractArchive(context.Background(), archive, dest, testLimits()); err == nil {
		t.Fatal("extractArchive() accepted a symlink entry")
	}
}

func TestArchiveExtractRejectsCaseInsensitiveDuplicates(t *testing.T) {
	archive := writeArchive(t, []zipEntry{
		{Name: "A.dcm", Data: []byte("one")},
		{Name: "a.dcm", Data: []byte("two")},
	})
	dest := filepath.Join(t.TempDir(), "dicom")
	if err := extractArchive(context.Background(), archive, dest, testLimits()); err == nil {
		t.Fatal("extractArchive() accepted case-insensitive duplicate targets")
	}
}

func TestArchiveExtractRejectsBaseNameCollisions(t *testing.T) {
	// Two directories may hold the same file name; flattening makes that
	// ambiguous, so it is rejected rather than silently overwriting.
	archive := writeArchive(t, []zipEntry{
		{Name: "study-a/CT000001.dcm", Data: []byte("one")},
		{Name: "study-b/CT000001.dcm", Data: []byte("two")},
	})
	dest := filepath.Join(t.TempDir(), "dicom")
	if err := extractArchive(context.Background(), archive, dest, testLimits()); err == nil {
		t.Fatal("extractArchive() accepted colliding base names")
	}
}

func TestArchiveExtractEnforcesPerFileLimit(t *testing.T) {
	archive := writeArchive(t, []zipEntry{{Name: "big.dcm", Data: make([]byte, 4096)}})
	dest := filepath.Join(t.TempDir(), "dicom")
	limits := testLimits()
	limits.fileBytes = 1024
	if err := extractArchive(context.Background(), archive, dest, limits); err == nil {
		t.Fatal("extractArchive() accepted a file over the per-file limit")
	}
	if _, err := os.Stat(filepath.Join(dest, "big.dcm")); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}
}

func TestArchiveExtractEnforcesTotalLimit(t *testing.T) {
	archive := writeArchive(t, []zipEntry{
		{Name: "one.dcm", Data: make([]byte, 800)},
		{Name: "two.dcm", Data: make([]byte, 800)},
	})
	dest := filepath.Join(t.TempDir(), "dicom")
	limits := testLimits()
	limits.totalBytes = 1000
	if err := extractArchive(context.Background(), archive, dest, limits); err == nil {
		t.Fatal("extractArchive() accepted data over the total limit")
	}
}

func TestArchiveExtractEnforcesEntryCount(t *testing.T) {
	entries := make([]zipEntry, 0, 4)
	for _, name := range []string{"a.dcm", "b.dcm", "c.dcm", "d.dcm"} {
		entries = append(entries, zipEntry{Name: name, Data: []byte("x")})
	}
	archive := writeArchive(t, entries)
	dest := filepath.Join(t.TempDir(), "dicom")
	limits := testLimits()
	limits.entries = 2
	if err := extractArchive(context.Background(), archive, dest, limits); err == nil {
		t.Fatal("extractArchive() accepted too many entries")
	}
}

func TestArchiveExtractRejectsDishonestDeclaredSize(t *testing.T) {
	// Declares 1 TiB but carries a single byte: the header check must reject it
	// before any copy, so disk is never touched.
	archive := writeArchive(t, []zipEntry{{
		Name:          "bomb.dcm",
		Data:          []byte("x"),
		raw:           true,
		declaredSize:  1 << 40,
		compressedLen: 1,
	}})
	dest := filepath.Join(t.TempDir(), "dicom")
	if err := extractArchive(context.Background(), archive, dest, testLimits()); err == nil {
		t.Fatal("extractArchive() accepted a dishonest declared size")
	}
}

func TestArchiveExtractHonoursCancellation(t *testing.T) {
	archive := writeArchive(t, []zipEntry{{Name: "a.dcm", Data: make([]byte, 2048)}})
	dest := filepath.Join(t.TempDir(), "dicom")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractArchive(ctx, archive, dest, testLimits()); err == nil {
		t.Fatal("extractArchive() ignored a canceled context")
	}
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		t.Fatalf("canceled extraction left files: %v", entries)
	}
}

func TestArchiveExtractKeepsFilesInsideRoot(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	dest := filepath.Join(root, "workspace", "dicom")

	archive := writeArchive(t, []zipEntry{{Name: "sub/a.dcm", Data: []byte("data")}})
	if err := extractArchive(context.Background(), archive, dest, testLimits()); err != nil {
		t.Fatalf("extractArchive() error = %v", err)
	}

	got, err := os.ReadFile(sentinel)
	if err != nil || string(got) != "untouched" {
		t.Fatalf("sentinel changed: %q, %v", got, err)
	}
	// The workspace root must contain only the extracted tree.
	workspaceRoot := filepath.Join(root, "workspace")
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatalf("read workspace root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "dicom" {
		t.Fatalf("workspace root entries = %v, want only dicom", entries)
	}
}

func TestArchiveEntryValidationAcceptsNestedRelativeNames(t *testing.T) {
	rel, isDir, err := safeArchiveEntry("a/b/c.dcm")
	if err != nil || isDir || rel != "a/b/c.dcm" {
		t.Fatalf("safeArchiveEntry = %q, %v, %v", rel, isDir, err)
	}
	if _, isDir, err := safeArchiveEntry("a/b/"); err != nil || !isDir {
		t.Fatalf("directory entry rejected: %v", err)
	}
	if !strings.Contains("a/b/c.dcm", "/") {
		t.Fatal("unreachable")
	}
}
