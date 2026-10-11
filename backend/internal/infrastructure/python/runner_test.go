package python

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/aijob"
)

// TestMain turns the test binary into a synthetic CLI helper when it is invoked
// with the exact argv shape the runner produces ("-m nodule_ai.infer ...").
// The helper is contract evidence for the orchestration only; it never loads a
// model and is not a product inference mode.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-m" {
		os.Exit(runHelper(os.Args))
	}
	if len(os.Args) > 1 && os.Args[1] == "-sleep-helper" {
		// A synthetic grandchild that keeps inherited output handles open.
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const helperSuccessJSON = `{"schema_version":"1","candidates":[]}`

func runHelper(args []string) int {
	flags := args[3:] // drop binary, "-m", module
	out := helperFlag(flags, "--out")
	series := helperFlag(flags, "--series-dir")

	mode, capture := readControl(series)
	if capture != "" {
		_ = os.WriteFile(capture, []byte(strings.Join(args[1:], "\n")), 0o600)
		if entries, err := os.ReadDir(series); err == nil {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			_ = os.WriteFile(capture+".listing", []byte(strings.Join(names, "\n")), 0o600)
		}
	}

	switch mode {
	case "success":
		return helperWrite(out, helperSuccessJSON, 0)
	case "exit2":
		return helperWrite(out, `{"schema_version":"1","error":{"code":"INVALID_DICOM","message":"safe"}}`, 2)
	case "exit2_unknown_code":
		return helperWrite(out, `{"schema_version":"1","error":{"code":"NO_SUCH_CODE","message":"safe"}}`, 2)
	case "exit2_missing":
		return 2
	case "exit3":
		return helperWrite(out, `{"schema_version":"1","error":{"code":"MODEL_OOM","message":"safe"}}`, 3)
	case "exit_other":
		return 7
	case "stdout_only":
		_, _ = io.WriteString(os.Stdout, helperSuccessJSON)
		return 0
	case "missing":
		return 0
	case "malformed":
		return helperWrite(out, "{not json", 0)
	case "oversized":
		return helperWrite(out, strings.Repeat("x", 5<<20), 0)
	case "stderr_secret":
		_, _ = io.WriteString(os.Stderr, "infer: MODEL_OOM\n/home/secret/bundle\nTraceback (most recent call last):\n")
		return 1
	case "stderr_flood":
		for i := 0; i < 4000; i++ {
			_, _ = io.WriteString(os.Stderr, "secret-token-line\n")
		}
		_, _ = io.WriteString(os.Stderr, "infer: NOT_CT\n")
		return 1
	case "block":
		time.Sleep(30 * time.Second)
		return 0
	case "lingering_child":
		// Write the result and exit 0, but leave a grandchild holding the
		// inherited stderr pipe so cmd.Wait returns exec.ErrWaitDelay.
		code := helperWrite(out, helperSuccessJSON, 0)
		grandchild := exec.Command(os.Args[0], "-sleep-helper")
		grandchild.Dir = os.TempDir()
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		_ = grandchild.Start()
		return code
	default:
		return 1
	}
}

func helperFlag(flags []string, name string) string {
	for index, flag := range flags {
		if flag == name && index+1 < len(flags) {
			return flags[index+1]
		}
		if value, ok := strings.CutPrefix(flag, name+"="); ok {
			return value
		}
	}
	return ""
}

// readControl reads "control.txt" from the extracted series directory: the
// first line is the mode and "capture=<path>" records the received argv.
func readControl(series string) (string, string) {
	if series == "" {
		return "", ""
	}
	raw, err := os.ReadFile(filepath.Join(series, "control.txt"))
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	mode := ""
	capture := ""
	if len(lines) > 0 {
		mode = strings.TrimSpace(lines[0])
	}
	for _, line := range lines[1:] {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "capture="); ok {
			capture = value
		}
	}
	return mode, capture
}

func helperWrite(path, content string, code int) int {
	if path != "" {
		_ = os.WriteFile(path, []byte(content), 0o600)
	}
	return code
}

// fakeArchive writes a fixed ZIP payload, or fails with an injected error.
type fakeArchive struct {
	payload []byte
	err     error
}

func (f fakeArchive) Download(_ context.Context, _ string, dst io.Writer) error {
	if f.err != nil {
		return f.err
	}
	_, err := dst.Write(f.payload)
	return err
}

// seriesArchive builds an in-memory ZIP whose extracted "control.txt" drives the
// helper process.
func seriesArchive(t *testing.T, mode, capture string, extra map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	control := mode + "\n"
	if capture != "" {
		control += "capture=" + capture + "\n"
	}
	files := map[string]string{"control.txt": control, "series/a.dcm": "dicom-bytes"}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buffer.Bytes()
}

func testRunner(t *testing.T, jobDir string) *Runner {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	return NewRunner(Settings{
		Python:    executable,
		BundleDir: t.TempDir(),
		JobDir:    jobDir,
		Device:    "cpu",
	})
}

func testJob() aijob.Job {
	return aijob.Job{
		ID:                  11,
		CaseID:              4,
		OrthancSeriesID:     "11111111-22222222-33333333-44444444-55555555",
		SeriesInstanceUID:   "1.2.3.4.5.6",
		FrameOfReferenceUID: "1.2.3.4.5.7",
	}
}

func TestRunnerPassesArgvWithSpacesAndReturnsResult(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "job root")
	capture := filepath.Join(t.TempDir(), "argv.txt")
	runner := testRunner(t, jobDir)
	archive := fakeArchive{payload: seriesArchive(t, "success", capture, nil)}

	execution, err := runner.Run(context.Background(), testJob(), archive)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if execution.ExitCode != 0 || string(execution.Result) != helperSuccessJSON {
		t.Fatalf("execution = %+v", execution)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read captured argv: %v", err)
	}
	argv := strings.Split(string(raw), "\n")
	want := []string{"-m", "nodule_ai.infer", "--series-dir", "", "--out", "", "--bundle-dir", runner.settings.BundleDir, "--score-threshold", "0.5", "--device", "cpu"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %d entries", argv, len(want))
	}
	for index, expected := range want {
		if expected == "" {
			continue
		}
		if argv[index] != expected {
			t.Fatalf("argv[%d] = %q, want %q", index, argv[index], expected)
		}
	}
	if !strings.Contains(argv[3], "job root") || !strings.HasSuffix(argv[3], "dicom") {
		t.Fatalf("series-dir argv = %q, want a spaced workspace path", argv[3])
	}
	if !strings.HasSuffix(argv[5], "result.json") {
		t.Fatalf("out argv = %q", argv[5])
	}

	// The helper's series directory must be flat: real Orthanc archives are
	// nested and the CLI lists without recursion.
	listing, err := os.ReadFile(capture + ".listing")
	if err != nil {
		t.Fatalf("read series listing: %v", err)
	}
	names := strings.Split(string(listing), "\n")
	if !containsString(names, "a.dcm") || !containsString(names, "control.txt") {
		t.Fatalf("series directory listing = %v, want flattened files", names)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestRunnerTreatsLingeringHandleAsCompletedProcess(t *testing.T) {
	// The CLI exits 0 with a valid result but a spawned grandchild keeps the
	// inherited stderr pipe open past WaitDelay. That is a completed process,
	// never a start failure: the result must survive.
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "lingering_child", "", nil)}

	execution, err := runner.Run(context.Background(), testJob(), archive)
	if err != nil {
		t.Fatalf("Run() error = %v, want a completed process", err)
	}
	if execution.ExitCode != 0 || string(execution.Result) != helperSuccessJSON {
		t.Fatalf("execution = %+v", execution)
	}
}

func TestRunnerStdoutOnlyIsRejected(t *testing.T) {
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "stdout_only", "", nil)}

	execution, err := runner.Run(context.Background(), testJob(), archive)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if execution.ExitCode != 0 || execution.Result != nil {
		t.Fatalf("execution = %+v, want exit 0 with no result", execution)
	}
}

func TestRunnerReturnsExitStatusSeparately(t *testing.T) {
	cases := map[string]struct {
		mode string
		code int
	}{
		"exit2":              {"exit2", 2},
		"exit2_missing":      {"exit2_missing", 2},
		"exit3":              {"exit3", 3},
		"exit_other":         {"exit_other", 7},
		"missing":            {"missing", 0},
		"malformed":          {"malformed", 0},
		"exit2_unknown_code": {"exit2_unknown_code", 2},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			runner := testRunner(t, t.TempDir())
			archive := fakeArchive{payload: seriesArchive(t, testCase.mode, "", nil)}
			execution, err := runner.Run(context.Background(), testJob(), archive)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if execution.ExitCode != testCase.code {
				t.Fatalf("ExitCode = %d, want %d", execution.ExitCode, testCase.code)
			}
		})
	}
}

func TestRunnerRejectsOversizedResult(t *testing.T) {
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "oversized", "", nil)}

	_, err := runner.Run(context.Background(), testJob(), archive)
	var codeErr *aijob.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeResultInvalid {
		t.Fatalf("Run() error = %v, want RESULT_INVALID", err)
	}
}

func TestRunnerSanitizesStderr(t *testing.T) {
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "stderr_secret", "", nil)}

	execution, err := runner.Run(context.Background(), testJob(), archive)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if execution.ExitCode != 1 {
		t.Fatalf("ExitCode = %d", execution.ExitCode)
	}
	if execution.StderrTail != "infer: MODEL_OOM\n[redacted]\n[redacted]" {
		t.Fatalf("StderrTail = %q", execution.StderrTail)
	}
	if strings.Contains(execution.StderrTail, "secret") {
		t.Fatalf("StderrTail leaked a path: %q", execution.StderrTail)
	}
}

func TestRunnerBoundsFloodedStderr(t *testing.T) {
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "stderr_flood", "", nil)}

	execution, err := runner.Run(context.Background(), testJob(), archive)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(execution.StderrTail) > aijob.MaxStderrTailBytes {
		t.Fatalf("StderrTail length = %d, want <= %d", len(execution.StderrTail), aijob.MaxStderrTailBytes)
	}
	if strings.Contains(execution.StderrTail, "secret-token-line") {
		t.Fatalf("StderrTail leaked flooded content")
	}
	if !strings.Contains(execution.StderrTail, "infer: NOT_CT") {
		t.Fatalf("StderrTail lost the allowlisted line: %q", execution.StderrTail)
	}
}

func TestRunnerTimesOutAndReaps(t *testing.T) {
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "block", "", nil)}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runner.Run(ctx, testJob(), archive)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run() took %v, process was not reaped", elapsed)
	}
	var codeErr *aijob.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeAITimeout {
		t.Fatalf("Run() error = %v, want AI_TIMEOUT", err)
	}
}

func TestRunnerCancellation(t *testing.T) {
	runner := testRunner(t, t.TempDir())
	archive := fakeArchive{payload: seriesArchive(t, "block", "", nil)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := runner.Run(ctx, testJob(), archive)
	var codeErr *aijob.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeAICanceled {
		t.Fatalf("Run() error = %v, want AI_CANCELED", err)
	}
}

func TestRunnerArchiveFailures(t *testing.T) {
	t.Run("download error", func(t *testing.T) {
		runner := testRunner(t, t.TempDir())
		_, err := runner.Run(context.Background(), testJob(), fakeArchive{err: errors.New("boom")})
		var codeErr *aijob.CodeError
		if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeOrthancArchiveError {
			t.Fatalf("Run() error = %v, want ORTHANC_ARCHIVE_ERROR", err)
		}
	})
	t.Run("invalid archive", func(t *testing.T) {
		runner := testRunner(t, t.TempDir())
		_, err := runner.Run(context.Background(), testJob(), fakeArchive{payload: []byte("not a zip")})
		var codeErr *aijob.CodeError
		if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeArchiveInvalid {
			t.Fatalf("Run() error = %v, want ARCHIVE_INVALID", err)
		}
	})
	t.Run("traversal archive", func(t *testing.T) {
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		entry, _ := writer.Create("../escape.dcm")
		_, _ = entry.Write([]byte("escape"))
		_ = writer.Close()

		root := t.TempDir()
		runner := testRunner(t, root)
		_, err := runner.Run(context.Background(), testJob(), fakeArchive{payload: buffer.Bytes()})
		var codeErr *aijob.CodeError
		if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeArchiveInvalid {
			t.Fatalf("Run() error = %v, want ARCHIVE_INVALID", err)
		}
		if _, statErr := os.Stat(filepath.Join(root, "escape.dcm")); !os.IsNotExist(statErr) {
			t.Fatalf("traversal wrote outside the workspace: %v", statErr)
		}
	})
}

func TestRunnerProcessStartFailure(t *testing.T) {
	runner := NewRunner(Settings{
		Python:    filepath.Join(t.TempDir(), "missing-interpreter"),
		BundleDir: t.TempDir(),
		JobDir:    t.TempDir(),
		Device:    "cpu",
	})
	_, err := runner.Run(context.Background(), testJob(), fakeArchive{payload: seriesArchive(t, "success", "", nil)})
	var codeErr *aijob.CodeError
	if !errors.As(err, &codeErr) || codeErr.Code != aijob.CodeProcessStartFailed {
		t.Fatalf("Run() error = %v, want PROCESS_START_FAILED", err)
	}
}

func TestRunnerFreshResultAndCleanup(t *testing.T) {
	jobDir := t.TempDir()
	runner := testRunner(t, jobDir)

	first, err := runner.Run(context.Background(), testJob(), fakeArchive{payload: seriesArchive(t, "success", "", nil)})
	if err != nil || first.Result == nil {
		t.Fatalf("first Run() = %+v, %v", first, err)
	}
	second, err := runner.Run(context.Background(), testJob(), fakeArchive{payload: seriesArchive(t, "exit_other", "", nil)})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if second.Result != nil {
		t.Fatalf("second Run() reused a stale result: %q", second.Result)
	}

	entries, err := os.ReadDir(jobDir)
	if err != nil {
		t.Fatalf("read job dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace not cleaned up: %v", entries)
	}
}

func TestRunnerReadResultFileRejectsSymlinkAndOversize(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	link := filepath.Join(dir, "result.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := readResultFile(link, resultFileLimit); err == nil {
		t.Fatal("readResultFile() followed a symlink")
	}

	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatalf("write big: %v", err)
	}
	if _, err := readResultFile(big, 10); err == nil {
		t.Fatal("readResultFile() accepted an oversized file")
	}

	missing := filepath.Join(dir, "absent.json")
	data, err := readResultFile(missing, resultFileLimit)
	if err != nil || data != nil {
		t.Fatalf("readResultFile(missing) = %v, %v; want nil, nil", data, err)
	}
}

func TestRunnerChildEnvDropsCredentials(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "db-secret")
	t.Setenv("ORTHANC_PASSWORD", "orthanc-secret")
	t.Setenv("AI_BUNDLE_DIR", "should-not-leak")

	env := childEnv()
	joined := strings.Join(env, "\n")
	for _, secret := range []string{"POSTGRES_PASSWORD", "ORTHANC_PASSWORD", "AI_BUNDLE_DIR", "db-secret", "orthanc-secret"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("childEnv leaked %q", secret)
		}
	}
	if os.Getenv("PATH") != "" && !strings.Contains(joined, "PATH=") {
		t.Fatal("childEnv dropped PATH")
	}
}
