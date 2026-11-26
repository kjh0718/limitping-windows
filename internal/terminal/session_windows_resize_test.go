//go:build windows

package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The child reports its own console size numerically, so the assertions do not
// depend on a localized `mode con` layout. powershell.exe ships with every
// Windows install; no real provider CLI is ever launched.
const psReportSize = `[Console]::Out.WriteLine('LPSIZE ' + [Console]::WindowWidth + 'x' + [Console]::WindowHeight + ' END')`

// psWaitForResize announces readiness, then reports the size once it differs
// from the one it started with (bounded at 15 s so a missed resize fails the
// test instead of hanging).
const psWaitForResize = `$w=[Console]::WindowWidth;$h=[Console]::WindowHeight;` +
	`[Console]::Out.WriteLine('LPREADY');` +
	`for($i=0;$i -lt 300;$i++){if([Console]::WindowWidth -ne $w -or [Console]::WindowHeight -ne $h){break};Start-Sleep -Milliseconds 50};` +
	`[Console]::Out.WriteLine('LPRESIZED ' + [Console]::WindowWidth + 'x' + [Console]::WindowHeight + ' END')`

func startPowerShell(t *testing.T, cols, rows int, script string) Session {
	t.Helper()
	sess, err := StartWithSize(context.Background(), exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script), cols, rows)
	if err != nil {
		t.Fatalf("StartWithSize(%dx%d) = %v", cols, rows, err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// expectSize waits for the `<prefix> WxH END` line the child prints and checks it.
func expectSize(t *testing.T, out *output, prefix string, cols, rows int) {
	t.Helper()
	re := regexp.MustCompile(prefix + ` (\d+)x(\d+) END`)
	want := fmt.Sprintf("%s %dx%d END", prefix, cols, rows)
	if !out.waitFor(want, outputTimeout) {
		got := "no report"
		if m := re.FindString(out.String()); m != "" {
			got = m
		}
		t.Fatalf("child never reported %q; %s (output %q)", want, got, out.String())
	}
}

func TestSessionPid(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	defer sess.Close()

	pid := sess.Pid()
	if pid <= 0 {
		t.Fatalf("Pid = %d, want a positive pid", pid)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d) = %v: Pid does not name a live process", pid, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		t.Fatalf("QueryFullProcessImageName = %v", err)
	}
	if exe := filepath.Base(windows.UTF16ToString(buf[:n])); !strings.EqualFold(exe, "cmd.exe") {
		t.Fatalf("Pid %d is %q, want the cmd.exe child", pid, exe)
	}
}

func TestStartWithSizeSetsInitialSize(t *testing.T) {
	sess := startPowerShell(t, 100, 40, psReportSize)
	expectSize(t, drain(sess), "LPSIZE", 100, 40)
}

// Start keeps the 120x30 default, which the ping paths rely on.
func TestStartKeepsDefaultSize(t *testing.T) {
	sess := startPowerShell(t, 0, 0, psReportSize)
	expectSize(t, drain(sess), "LPSIZE", 120, 30)
}

func TestSessionResizeIsObservedByChild(t *testing.T) {
	sess := startPowerShell(t, 80, 24, psWaitForResize)
	out := drain(sess)
	if !out.waitFor("LPREADY", outputTimeout) {
		t.Fatalf("child never became ready; got %q", out.String())
	}
	if err := sess.Resize(132, 43); err != nil {
		t.Fatalf("Resize = %v", err)
	}
	expectSize(t, out, "LPRESIZED", 132, 43)
}

func TestSessionResizeRejectsInvalidSizes(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	defer sess.Close()
	for _, c := range [][2]int{{0, 10}, {10, 0}, {-1, 5}, {5, -1}, {MaxTerminalDim + 1, 5}, {5, MaxTerminalDim + 1}} {
		err := sess.Resize(c[0], c[1])
		if err == nil || errors.Is(err, os.ErrClosed) {
			t.Errorf("Resize(%d, %d) = %v, want a size validation error", c[0], c[1], err)
		}
	}
	if err := sess.Resize(90, 30); err != nil {
		t.Errorf("Resize(90, 30) on a live session = %v", err)
	}
}

// After Close the HPCON is freed; Resize must report os.ErrClosed rather than
// touch it.
func TestSessionResizeAfterCloseIsErrClosed(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	if err := sess.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := sess.Resize(100, 40); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Resize after Close = %v, want os.ErrClosed", err)
	}
	waitResult(sess, waitTimeout)
}

// The waiter closes the pseudoconsole when the child exits, so a resize racing
// the end of the child gets the same os.ErrClosed instead of a dead handle.
func TestSessionResizeAfterChildExitIsErrClosed(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, "/c", "exit 0")
	defer sess.Close()
	if _, ok := waitResult(sess, waitTimeout); !ok {
		t.Fatal("Wait did not return")
	}
	if err := sess.Resize(100, 40); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Resize after exit = %v, want os.ErrClosed", err)
	}
}

// Resize holds the HPCON lock across the whole ResizePseudoConsole call, and the
// close takes it before freeing the console. This hammers Resize against Wait
// and Close; a use-after-free would crash or hang here, so every step is bounded.
func TestSessionConcurrentResizeWaitClose(t *testing.T) {
	for i := 0; i < 8; i++ {
		sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
		out := drain(sess)

		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for n := 0; ; n++ {
					select {
					case <-stop:
						return
					default:
					}
					if err := sess.Resize(60+(n+g)%60, 20+(n+g)%20); err != nil && !errors.Is(err, os.ErrClosed) {
						t.Errorf("iteration %d: Resize = %v", i, err)
						return
					}
				}
			}(g)
		}
		time.Sleep(50 * time.Millisecond)
		waitDone := make(chan struct{})
		go func() { sess.Wait(); close(waitDone) }()
		_ = sess.Kill()
		_ = sess.Close()
		select {
		case <-waitDone:
		case <-time.After(waitTimeout):
			t.Fatalf("iteration %d: Wait did not return", i)
		}
		close(stop)
		wg.Wait()
		if !out.waitFinished(waitTimeout) {
			t.Fatalf("iteration %d: Read stayed blocked", i)
		}
		if err := sess.Resize(80, 24); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("iteration %d: Resize after close = %v, want os.ErrClosed", i, err)
		}
	}
}

// The close of the pseudoconsole must wait for a resize that is already inside
// ResizePseudoConsole (the call would otherwise run on a freed HPCON), must not
// make the closer's caller wait for it, and must turn later resizes away.
func TestConPTYCloseWaitsForResizeInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls int32
	old := resizePseudoConsole
	resizePseudoConsole = func(windows.Handle, windows.Coord) error {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	defer func() { resizePseudoConsole = old }()

	// The close is observed at its entry, not its return: ClosePseudoConsole may
	// block for a long time, so "has it returned" cannot show it had not started.
	closeEntered := make(chan struct{})
	oldClose := closePseudoConsole
	closePseudoConsole = func(h windows.Handle) {
		close(closeEntered)
		oldClose(h)
	}
	defer func() { closePseudoConsole = oldClose }()

	pty, err := newConPTY(conPTYSize)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.teardown()
	var releaseOnce sync.Once
	releaseResize := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseResize() // runs before teardown, so a failing test cannot hang on the parked resize

	resizeDone := make(chan error, 1)
	go func() { resizeDone <- pty.resize(windows.Coord{X: 100, Y: 40}) }()
	<-entered

	returned := make(chan (<-chan struct{}), 1)
	go func() { returned <- pty.closeHPCON() }()
	var closed <-chan struct{}
	select {
	case closed = <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("closeHPCON blocked its caller behind a resize in flight")
	}
	select {
	case <-closeEntered:
		t.Fatal("ClosePseudoConsole was entered while a resize was still inside ResizePseudoConsole")
	case <-time.After(200 * time.Millisecond):
	}
	if err := pty.resize(windows.Coord{X: 90, Y: 30}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("resize after the close was requested = %v, want os.ErrClosed", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("ResizePseudoConsole called %d times, want only the in-flight one", n)
	}

	releaseResize()
	if err := <-resizeDone; err != nil {
		t.Fatalf("in-flight resize = %v", err)
	}
	select {
	case <-closeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("ClosePseudoConsole never ran after the resize finished")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("ClosePseudoConsole never returned")
	}
}

func TestWin32FromHRESULT(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want error
	}{
		{"NO_DATA as HRESULT", syscall.Errno(0x800700E8), windows.ERROR_NO_DATA},
		{"BROKEN_PIPE as HRESULT", syscall.Errno(0x8007006D), windows.ERROR_BROKEN_PIPE},
		{"INVALID_PARAMETER as HRESULT passes to its Win32 code", syscall.Errno(0x80070057), windows.ERROR_INVALID_PARAMETER},
		{"plain Win32 code unchanged", windows.ERROR_NO_DATA, windows.ERROR_NO_DATA},
		{"other facility unchanged", syscall.Errno(0x80040154), syscall.Errno(0x80040154)},
		{"non-errno unchanged", os.ErrClosed, os.ErrClosed},
	}
	for _, c := range cases {
		if got := win32FromHRESULT(c.in); got != c.want {
			t.Errorf("%s: win32FromHRESULT(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
	// The mapping must be what makes resize treat a dying console host as closed.
	got := win32FromHRESULT(syscall.Errno(0x800700E8))
	if !errors.Is(got, windows.ERROR_NO_DATA) || !(isPipeEOF(got) || errors.Is(got, windows.ERROR_NO_DATA)) {
		t.Errorf("0x800700E8 does not reach the closed-console path: %v", got)
	}
}
