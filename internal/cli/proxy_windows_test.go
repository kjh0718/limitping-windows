//go:build windows

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// --- watchResize -----------------------------------------------------------

type sizeStep struct {
	cols, rows int
	err        error
}

// scriptedSize returns the steps in order, then repeats the last one.
func scriptedSize(steps []sizeStep) func() (int, int, error) {
	var mu sync.Mutex
	i := 0
	return func() (int, int, error) {
		mu.Lock()
		defer mu.Unlock()
		s := steps[min(i, len(steps)-1)]
		i++
		return s.cols, s.rows, s.err
	}
}

type resizeCall struct{ cols, rows int }

type resizeRecorder struct {
	mu    sync.Mutex
	calls []resizeCall
	errs  []error // returned in order; nil once exhausted
}

func (r *resizeRecorder) resize(c, rw int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, resizeCall{c, rw})
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		return err
	}
	return nil
}

func (r *resizeRecorder) snapshot() []resizeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]resizeCall(nil), r.calls...)
}

// runWatchResize runs watchResize with a 1 ms poll for d, then stops it.
func runWatchResize(t *testing.T, d time.Duration, cols, rows int, size func() (int, int, error), rec *resizeRecorder) {
	t.Helper()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		watchResize(done, time.Millisecond, cols, rows, size, rec.resize, nil)
	}()
	time.Sleep(d)
	close(done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("watchResize did not stop after done was closed")
	}
}

func TestWatchResizeCallsOnlyOnChange(t *testing.T) {
	rec := &resizeRecorder{}
	size := scriptedSize([]sizeStep{{80, 24, nil}, {80, 24, nil}, {100, 30, nil}, {100, 30, nil}})
	runWatchResize(t, 100*time.Millisecond, 80, 24, size, rec)
	got := rec.snapshot()
	if len(got) != 1 || got[0] != (resizeCall{100, 30}) {
		t.Fatalf("resize calls = %v, want exactly [{100 30}]", got)
	}
}

func TestWatchResizeIgnoresUnreadableAndEmptySizes(t *testing.T) {
	rec := &resizeRecorder{}
	size := scriptedSize([]sizeStep{{0, 0, errors.New("boom")}, {0, 0, nil}, {-1, 5, nil}, {120, 0, nil}, {80, 24, nil}})
	runWatchResize(t, 100*time.Millisecond, 80, 24, size, rec)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("resize calls = %v, want none for unreadable/empty/unchanged sizes", got)
	}
}

// A size the pseudoconsole rejected must not be remembered as applied, or the
// terminal would stay at the old size until the user resizes again.
func TestWatchResizeRetriesAfterFailedResize(t *testing.T) {
	rec := &resizeRecorder{errs: []error{errors.New("transient")}}
	runWatchResize(t, 100*time.Millisecond, 80, 24, scriptedSize([]sizeStep{{100, 30, nil}}), rec)
	got := rec.snapshot()
	if len(got) != 2 || got[0] != (resizeCall{100, 30}) || got[1] != (resizeCall{100, 30}) {
		t.Fatalf("resize calls = %v, want the failed size retried once and then recorded", got)
	}
}

func TestWatchResizeStopsOnErrClosed(t *testing.T) {
	rec := &resizeRecorder{errs: []error{os.ErrClosed}}
	done := make(chan struct{})
	defer close(done)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		watchResize(done, time.Millisecond, 80, 24, scriptedSize([]sizeStep{{100, 30, nil}}), rec.resize, nil)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("watchResize kept polling a closed session")
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("resize calls = %v, want a single call before stopping", got)
	}
}

// A session that started without a known size (the initial lookup failed) is
// sized by the first poll that works.
func TestWatchResizeSizesASessionThatStartedWithoutOne(t *testing.T) {
	rec := &resizeRecorder{}
	runWatchResize(t, 100*time.Millisecond, 0, 0, scriptedSize([]sizeStep{{0, 0, errors.New("no console yet")}, {110, 33, nil}}), rec)
	if got := rec.snapshot(); len(got) != 1 || got[0] != (resizeCall{110, 33}) {
		t.Fatalf("resize calls = %v, want [{110 33}]", got)
	}
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logRecorder) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// A persistent failure is logged once; a different failure, or the same one after
// a success, is logged again.
func TestWatchResizeLogsFailureKindsOnce(t *testing.T) {
	rec := &resizeRecorder{errs: []error{errors.New("A"), errors.New("A"), errors.New("A"), errors.New("B"), errors.New("B"), nil, errors.New("A")}}
	lr := &logRecorder{}
	done := make(chan struct{})
	finished := make(chan struct{})
	// Different sizes every poll so each tick really calls resize.
	var n int
	var mu sync.Mutex
	size := func() (int, int, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return 100 + n, 30, nil
	}
	go func() {
		defer close(finished)
		watchResize(done, time.Millisecond, 80, 24, size, rec.resize, lr.logf)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.snapshot()) < 8 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(done)
	<-finished

	lines := lr.snapshot()
	if len(lines) != 3 || !strings.HasSuffix(lines[0], ": A") || !strings.HasSuffix(lines[1], ": B") || !strings.HasSuffix(lines[2], ": A") {
		t.Fatalf("log lines = %q, want one each for A, B, and A again after a success", lines)
	}
}

func TestWatchResizeLogsUnreadableSizeOnce(t *testing.T) {
	lr := &logRecorder{}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		watchResize(done, time.Millisecond, 80, 24, scriptedSize([]sizeStep{{0, 0, errors.New("no screen buffer")}}), (&resizeRecorder{}).resize, lr.logf)
	}()
	time.Sleep(100 * time.Millisecond)
	close(done)
	<-finished
	if lines := lr.snapshot(); len(lines) != 1 {
		t.Fatalf("log lines = %q, want exactly one", lines)
	}
}

// --- pumpStdin -------------------------------------------------------------

// scriptReader replays steps; once they run out it returns (0, io.EOF) forever
// and counts the reads.
type scriptReader struct {
	mu    sync.Mutex
	steps []scriptRead
	reads int
}

type scriptRead struct {
	data string
	err  error
}

func (r *scriptReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	s := r.steps[0]
	r.steps = r.steps[1:]
	return copy(p, s.data), s.err
}

func (r *scriptReader) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func withFastStdinRetry(t *testing.T, min, max time.Duration) {
	t.Helper()
	oldMin, oldMax := stdinRetryMin, stdinRetryMax
	stdinRetryMin, stdinRetryMax = min, max
	t.Cleanup(func() { stdinRetryMin, stdinRetryMax = oldMin, oldMax })
}

func TestPumpStdinConsoleSurvivesEOF(t *testing.T) {
	withFastStdinRetry(t, time.Millisecond, 4*time.Millisecond)
	boom := errors.New("boom")
	src := &scriptReader{steps: []scriptRead{
		{"a", io.EOF}, {"", io.EOF}, {"b", nil}, {"", io.EOF}, {"c", boom},
	}}
	var dst bytes.Buffer
	if err := pumpStdin(make(chan struct{}), &dst, src, true); !errors.Is(err, boom) {
		t.Fatalf("pumpStdin = %v, want the read error", err)
	}
	if got := dst.String(); got != "abc" {
		t.Fatalf("forwarded %q, want \"abc\": input after a Ctrl+Z EOF was lost", got)
	}
}

func TestPumpStdinNonConsoleStopsAtEOF(t *testing.T) {
	src := &scriptReader{steps: []scriptRead{{"abc", nil}, {"", io.EOF}, {"late", nil}}}
	var dst bytes.Buffer
	if err := pumpStdin(make(chan struct{}), &dst, src, false); err != nil {
		t.Fatalf("pumpStdin = %v, want nil at EOF", err)
	}
	if dst.String() != "abc" {
		t.Fatalf("forwarded %q, want \"abc\": a pipe's EOF must end the pump", dst.String())
	}
}

func TestPumpStdinStopsOnWriteError(t *testing.T) {
	withFastStdinRetry(t, time.Millisecond, 4*time.Millisecond)
	src := &scriptReader{steps: []scriptRead{{"a", io.EOF}, {"b", io.EOF}}}
	err := pumpStdin(make(chan struct{}), failingWriter{os.ErrClosed}, src, true)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("pumpStdin = %v, want the write error", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// A console that returns EOF forever must neither spin nor outlive done.
func TestPumpStdinConsolePermanentEOFBacksOffAndStops(t *testing.T) {
	withFastStdinRetry(t, 5*time.Millisecond, 40*time.Millisecond)
	src := &scriptReader{}
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() { finished <- pumpStdin(done, io.Discard, src, true) }()

	time.Sleep(300 * time.Millisecond)
	// Delays go 10, 20, 40, 40, ... ms, so 300 ms allows well under 20 reads; a
	// loop without backoff would make many thousands.
	if n := src.readCount(); n > 30 {
		t.Fatalf("%d reads in 300 ms: the EOF restart is busy-looping", n)
	}
	close(done)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("pumpStdin = %v, want nil when done closes", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pumpStdin ignored done")
	}
}

// --- enableVT / console setup ---------------------------------------------

func TestEnableVTFailsOnNonConsole(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notaconsole")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	restore, err := enableVT(windows.Handle(f.Fd()))
	if err == nil {
		restore()
		t.Fatal("enableVT succeeded on a regular file")
	}
	if restore != nil {
		t.Fatal("enableVT returned a restore func alongside its error")
	}
}

type fakeConsole struct {
	mode  uint32
	sets  []uint32
	fails func(mode uint32) bool
}

func (c *fakeConsole) get(_ windows.Handle, m *uint32) error { *m = c.mode; return nil }
func (c *fakeConsole) set(_ windows.Handle, m uint32) error {
	c.sets = append(c.sets, m)
	if c.fails != nil && c.fails(m) {
		return errors.New("rejected")
	}
	c.mode = m
	return nil
}

func TestEnableVTModeSetsFlagsAndRestores(t *testing.T) {
	c := &fakeConsole{mode: 0x10}
	restore, err := enableVTMode(0, c.get, c.set)
	if err != nil {
		t.Fatal(err)
	}
	want := uint32(0x10) | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
	if c.mode != want {
		t.Fatalf("mode = %#x, want %#x", c.mode, want)
	}
	restore()
	if c.mode != 0x10 {
		t.Fatalf("mode after restore = %#x, want the original 0x10", c.mode)
	}
}

func TestEnableVTModeFallsBackWithoutNewlineAutoReturn(t *testing.T) {
	c := &fakeConsole{mode: 0, fails: func(m uint32) bool { return m&windows.DISABLE_NEWLINE_AUTO_RETURN != 0 }}
	restore, err := enableVTMode(0, c.get, c.set)
	if err != nil {
		t.Fatalf("enableVTMode = %v, want a retry without DISABLE_NEWLINE_AUTO_RETURN", err)
	}
	if c.mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 || c.mode&windows.DISABLE_NEWLINE_AUTO_RETURN != 0 {
		t.Fatalf("mode = %#x, want VT processing without DISABLE_NEWLINE_AUTO_RETURN", c.mode)
	}
	restore()
	if c.mode != 0 {
		t.Fatalf("mode after restore = %#x, want 0", c.mode)
	}
}

func TestEnableVTModeFailsWhenVTIsRejected(t *testing.T) {
	c := &fakeConsole{fails: func(uint32) bool { return true }}
	if restore, err := enableVTMode(0, c.get, c.set); err == nil || restore != nil {
		t.Fatalf("enableVTMode = %v, restore=%v, want an error and no restore func", err, restore != nil)
	}
}

func TestSetupConsoleSkipsRedirectedStreams(t *testing.T) {
	restore, err := setupConsole(false, false)
	if err != nil {
		t.Fatalf("setupConsole = %v, want nil when nothing is a console", err)
	}
	restore() // must be safe
	if c, r := consoleSize(false); c != 0 || r != 0 {
		t.Fatalf("consoleSize(false) = %dx%d, want 0x0", c, r)
	}
}

// --- shim pid resolution ---------------------------------------------------

func TestShimChildPID(t *testing.T) {
	const root = 100
	cmdRoot := procEntry{root, 1, "cmd.exe"}
	cases := []struct {
		name  string
		procs []procEntry
		want  int
	}{
		{"npm shape: node child", []procEntry{cmdRoot, {200, root, "node.exe"}}, 200},
		{"claude.exe child, any case", []procEntry{cmdRoot, {201, root, "Claude.EXE"}}, 201},
		{"unrelated children ignored", []procEntry{cmdRoot, {202, root, "conhost.exe"}, {203, root, "node.exe"}}, 203},
		{"grandchildren ignored", []procEntry{cmdRoot, {204, 999, "node.exe"}}, 0},
		{"no child yet", []procEntry{cmdRoot}, 0},
		{"two candidates are ambiguous", []procEntry{cmdRoot, {205, root, "node.exe"}, {206, root, "claude.exe"}}, 0},
		{"root is not cmd.exe (pid reuse)", []procEntry{{root, 1, "explorer.exe"}, {207, root, "node.exe"}}, 0},
		{"root vanished", []procEntry{{207, root, "node.exe"}}, 0},
	}
	for _, c := range cases {
		if got := shimChildPID(c.procs, root); got != c.want {
			t.Errorf("%s: shimChildPID = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestClaudeProcessPID(t *testing.T) {
	lg := &proxyLogger{}
	tree := []procEntry{{100, 1, "cmd.exe"}, {200, 100, "node.exe"}}
	list := func() ([]procEntry, error) { return tree, nil }
	neverList := func() ([]procEntry, error) { t.Fatal("process list consulted for a native exe"); return nil, nil }

	if got := claudeProcessPID(`C:\bin\claude.exe`, 100, neverList, lg); got != 100 {
		t.Errorf("native exe: pid = %d, want the session's own 100", got)
	}
	for _, path := range []string{`C:\npm\claude.cmd`, `C:\npm\CLAUDE.BAT`} {
		if got := claudeProcessPID(path, 100, list, lg); got != 200 {
			t.Errorf("%s: pid = %d, want the shim's node child 200", path, got)
		}
	}
	if got := claudeProcessPID(`C:\npm\claude.cmd`, 999, list, lg); got != -1 {
		t.Errorf("unknown root: pid = %d, want -1 (matches no session)", got)
	}
	failing := func() ([]procEntry, error) { return nil, errors.New("snapshot failed") }
	if got := claudeProcessPID(`C:\npm\claude.cmd`, 100, failing, lg); got != -1 {
		t.Errorf("list error: pid = %d, want -1", got)
	}
}

// The real snapshot wrapper must see this very process with its parent.
func TestListProcessesFindsSelf(t *testing.T) {
	procs, err := listProcesses()
	if err != nil {
		t.Fatalf("listProcesses = %v", err)
	}
	for _, p := range procs {
		if p.pid == os.Getpid() {
			if p.ppid != os.Getppid() || !strings.HasSuffix(strings.ToLower(p.exe), ".exe") {
				t.Fatalf("self entry = %+v, want ppid %d and an .exe name", p, os.Getppid())
			}
			return
		}
	}
	t.Fatalf("process %d missing from %d snapshot entries", os.Getpid(), len(procs))
}
