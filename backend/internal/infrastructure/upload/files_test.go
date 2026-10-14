package upload_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"mime/multipart"
	"net/textproto"
	"os"
	"testing"

	"nodule-reader/backend/internal/infrastructure/upload"
)

// buildMultipart writes the given named parts into one multipart body so the
// stager reads exactly what an HTTP request would carry.
func buildMultipart(t *testing.T, parts []part) *multipart.Reader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, p := range parts {
		field := "files"
		if p.field != "" {
			field = p.field
		}
		header := textproto.MIMEHeader{
			"Content-Disposition": []string{
				`form-data; name="` + field + `"; filename="` + p.name + `"`,
			},
		}
		w, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := w.Write(p.data); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return multipart.NewReader(&body, writer.Boundary())
}

type part struct {
	name  string
	data  []byte
	field string
}

func stage(t *testing.T, limits upload.Limits, parts ...part) (*upload.Workspace, error) {
	t.Helper()
	root := t.TempDir()
	return upload.Stage(context.Background(), root, buildMultipart(t, parts), limits)
}

func stagedBytes(t *testing.T, ws *upload.Workspace) [][]byte {
	t.Helper()
	files := ws.Files()
	out := make([][]byte, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("read staged file: %v", err)
		}
		if int64(len(raw)) != f.Size {
			t.Fatalf("staged size %d does not match %d bytes", f.Size, len(raw))
		}
		out = append(out, raw)
	}
	return out
}

func zipBytes(t *testing.T, entries map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range order {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := f.Write(entries[name]); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func tarBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), Typeflag: e.typeflag}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := w.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := w.Write(e.data); err != nil {
				t.Fatalf("tar write: %v", err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	name     string
	data     []byte
	typeflag byte
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestStageAcceptsExtensionlessDICOM(t *testing.T) {
	ws, err := stage(t, upload.DefaultLimits(),
		part{name: "scan", data: []byte("DICM-raw-a")},
		part{name: "scan2", data: []byte("DICM-raw-b")},
	)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer ws.Cleanup()
	if len(ws.Files()) != 2 {
		t.Fatalf("want 2 staged files, got %d", len(ws.Files()))
	}
	if ws.SkippedFiles() != 0 {
		t.Fatalf("want 0 skipped, got %d", ws.SkippedFiles())
	}
	got := stagedBytes(t, ws)
	if string(got[0]) != "DICM-raw-a" || string(got[1]) != "DICM-raw-b" {
		t.Fatalf("staged bytes changed: %q", got)
	}
}

func TestStageExtractsZipWithNestedDirectories(t *testing.T) {
	entries := map[string][]byte{
		"study/series/a.dcm": []byte("aaa"),
		"b.dcm":              []byte("bbb"),
	}
	archive := zipBytes(t, entries, []string{"study/series/a.dcm", "b.dcm"})
	ws, err := stage(t, upload.DefaultLimits(), part{name: "batch.zip", data: archive})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer ws.Cleanup()
	if len(ws.Files()) != 2 {
		t.Fatalf("want 2 staged files, got %d", len(ws.Files()))
	}
	seen := map[string]bool{}
	for _, raw := range stagedBytes(t, ws) {
		seen[string(raw)] = true
	}
	if !seen["aaa"] || !seen["bbb"] {
		t.Fatalf("archive contents not extracted: %v", seen)
	}
}

func TestStageExtractsTarAndTarGz(t *testing.T) {
	raw := tarBytes(t, []tarEntry{{name: "dir/a.dcm", data: []byte("t1")}, {name: "b.dcm", data: []byte("t2")}})
	for name, data := range map[string][]byte{"plain.tar": raw, "plain.tar.gz": gzipBytes(t, raw)} {
		ws, err := stage(t, upload.DefaultLimits(), part{name: name, data: data})
		if err != nil {
			t.Fatalf("%s: stage: %v", name, err)
		}
		if len(ws.Files()) != 2 {
			t.Fatalf("%s: want 2 staged files, got %d", name, len(ws.Files()))
		}
		ws.Cleanup()
	}
}

func TestStageSkipsIndexAndSidecarEntries(t *testing.T) {
	archive := zipBytes(t, map[string][]byte{
		"DICOMDIR":         []byte("index"),
		"study/.DS_Store":  []byte("junk"),
		"study/Thumbs.db":  []byte("junk"),
		"__MACOSX/._a.dcm": []byte("resource fork"),
		"study/real.dcm":   []byte("real"),
	}, []string{"DICOMDIR", "study/.DS_Store", "study/Thumbs.db", "__MACOSX/._a.dcm", "study/real.dcm"})
	ws, err := stage(t, upload.DefaultLimits(), part{name: "export.zip", data: archive})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer ws.Cleanup()
	if len(ws.Files()) != 1 {
		t.Fatalf("want 1 staged file, got %d", len(ws.Files()))
	}
	if ws.SkippedFiles() != 4 {
		t.Fatalf("want 4 skipped, got %d", ws.SkippedFiles())
	}
}

func TestStageRejectsUnsafeEntryNames(t *testing.T) {
	for _, name := range []string{"../evil.dcm", "/etc/evil.dcm", "..\\evil.dcm", "C:/evil.dcm", "//host/share/evil.dcm"} {
		archive := zipBytes(t, map[string][]byte{name: []byte("x")}, []string{name})
		_, err := stage(t, upload.DefaultLimits(), part{name: "bad.zip", data: archive})
		if !errors.Is(err, upload.ErrUnsafeEntry) {
			t.Fatalf("%q: want ErrUnsafeEntry, got %v", name, err)
		}
	}
}

func TestStageRejectsSymlinkEntry(t *testing.T) {
	raw := tarBytes(t, []tarEntry{{name: "link", typeflag: tar.TypeSymlink}})
	if _, err := stage(t, upload.DefaultLimits(), part{name: "links.tar", data: raw}); !errors.Is(err, upload.ErrUnsafeEntry) {
		t.Fatalf("want ErrUnsafeEntry, got %v", err)
	}
}

func TestStageRejectsEncryptedZip(t *testing.T) {
	archive := zipBytes(t, map[string][]byte{"a.dcm": []byte("x")}, []string{"a.dcm"})
	// Set the general-purpose encryption bit in the local header and the
	// central directory so the reader sees an encrypted entry.
	archive = setZipEncrypted(archive, t)
	if _, err := stage(t, upload.DefaultLimits(), part{name: "enc.zip", data: archive}); !errors.Is(err, upload.ErrUnsupportedFormat) {
		t.Fatalf("want ErrUnsupportedFormat, got %v", err)
	}
}

func TestStageRejectsNestedArchive(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{"a.dcm": []byte("x")}, []string{"a.dcm"})
	outer := zipBytes(t, map[string][]byte{"inner.zip": inner}, []string{"inner.zip"})
	if _, err := stage(t, upload.DefaultLimits(), part{name: "outer.zip", data: outer}); !errors.Is(err, upload.ErrNestedArchive) {
		t.Fatalf("want ErrNestedArchive, got %v", err)
	}
}

func TestStageRejectsTruncatedArchive(t *testing.T) {
	archive := zipBytes(t, map[string][]byte{"a.dcm": []byte("payload")}, []string{"a.dcm"})
	if _, err := stage(t, upload.DefaultLimits(), part{name: "cut.zip", data: archive[:len(archive)-8]}); !errors.Is(err, upload.ErrMalformedArchive) {
		t.Fatalf("want ErrMalformedArchive, got %v", err)
	}
}

func TestStageEnforcesByteLimits(t *testing.T) {
	limits := upload.DefaultLimits()
	limits.MaxFileBytes = 4
	if _, err := stage(t, limits, part{name: "big.dcm", data: []byte("0123456789")}); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("file budget: want ErrTooLarge, got %v", err)
	}
	limits = upload.DefaultLimits()
	limits.MaxTotalBytes = 6
	if _, err := stage(t, limits, part{name: "a.dcm", data: []byte("aaaa")}, part{name: "b.dcm", data: []byte("bbbb")}); !errors.Is(err, upload.ErrTooLarge) {
		t.Fatalf("total budget: want ErrTooLarge, got %v", err)
	}
}

func TestStageEnforcesEntryAndFileLimits(t *testing.T) {
	limits := upload.DefaultLimits()
	limits.MaxEntries = 2
	archive := zipBytes(t, map[string][]byte{"a.dcm": []byte("a"), "b.dcm": []byte("b")}, []string{"a.dcm", "b.dcm"})
	if _, err := stage(t, limits, part{name: "many.zip", data: archive}); !errors.Is(err, upload.ErrTooManyEntries) {
		t.Fatalf("entry budget: want ErrTooManyEntries, got %v", err)
	}
	limits = upload.DefaultLimits()
	limits.MaxFiles = 1
	if _, err := stage(t, limits, part{name: "a.dcm", data: []byte("a")}, part{name: "b.dcm", data: []byte("b")}); !errors.Is(err, upload.ErrTooManyFiles) {
		t.Fatalf("file budget: want ErrTooManyFiles, got %v", err)
	}
}

func TestStageRejectsUnknownField(t *testing.T) {
	if _, err := stage(t, upload.DefaultLimits(), part{name: "a", field: "other", data: []byte("x")}); !errors.Is(err, upload.ErrUnknownField) {
		t.Fatalf("want ErrUnknownField, got %v", err)
	}
}

func TestStageHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	_, err := upload.Stage(ctx, root, buildMultipart(t, []part{{name: "a.dcm", data: []byte("x")}}), upload.DefaultLimits())
	if !errors.Is(err, upload.ErrCanceled) {
		t.Fatalf("want ErrCanceled, got %v", err)
	}
	assertEmptyDir(t, root)
}

func TestStageCleansWorkspaceOnFailure(t *testing.T) {
	root := t.TempDir()
	archive := zipBytes(t, map[string][]byte{"../evil.dcm": []byte("x")}, []string{"../evil.dcm"})
	if _, err := upload.Stage(context.Background(), root, buildMultipart(t, []part{{name: "bad.zip", data: archive}}), upload.DefaultLimits()); err == nil {
		t.Fatal("want an error")
	}
	assertEmptyDir(t, root)
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("workspace not cleaned, left %v", names)
	}
}

// setZipEncrypted flips the encryption bit in a stored, uncompressed zip so the
// stager can be tested against an encrypted entry without a real encryptor.
func setZipEncrypted(archive []byte, t *testing.T) []byte {
	t.Helper()
	out := append([]byte(nil), archive...)
	local := bytes.Index(out, []byte{'P', 'K', 3, 4})
	central := bytes.Index(out, []byte{'P', 'K', 1, 2})
	if local < 0 || central < 0 {
		t.Fatal("zip signatures not found")
	}
	out[local+6] |= 0x1
	out[central+8] |= 0x1
	return out
}
