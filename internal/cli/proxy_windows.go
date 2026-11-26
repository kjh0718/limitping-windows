//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"github.com/wavever/CCLimitPing/internal/config"
	"github.com/wavever/CCLimitPing/internal/terminal"
)

const (
	// proxyResizePoll is how often the real console size is compared with the
	// pseudoconsole's: Windows has no SIGWINCH, and a console resize is only
	// visible by asking.
	proxyResizePoll = 250 * time.Millisecond
	// proxyDrainTimeout bounds the wait for the output pump to flush the child's
	// last bytes (including its alternate-screen exit) once the child has exited.
	proxyDrainTimeout = time.Second
	// proxyStopTimeout bounds the wait for the output pump after the session has
	// been closed, which is what unblocks it if a grandchild kept the console busy.
	proxyStopTimeout = 2 * time.Second
)

// Backoff for pumpStdin's restart after an EOF on a console; variables so tests
// can shorten them.
var (
	stdinRetryMin = 20 * time.Millisecond
	stdinRetryMax = time.Second
)

// runContinueProxy launches providerName's interactive CLI (plus any passthrough
// extraArgs, e.g. --yolo) under a ConPTY, wiring the user's console straight
// through, while watchAndContinue injects the continue message whenever the 5h
// limit recovers after being hit. providerName must be "claude" or "codex".
//
// Windows differences from proxy_unix.go: the console is switched to VT output
// and raw input by hand, there is no SIGWINCH (so the size is polled), and there
// is no graceful-stop signal for the child, so cancellation kills the whole job.
func runContinueProxy(ctx context.Context, out io.Writer, providerName string, extraArgs []string, cfg config.Config) error {
	p, err := buildProvider(providerName, cfg)
	if err != nil {
		return err
	}
	msg := continueMessage(cfg, providerName)

	lg := newProxyLogger()
	defer lg.close()

	// Ctrl+Break and closing the console window arrive as os.Interrupt and
	// SIGTERM; either cancels ctx, which kills the child's job. Ctrl+C is NOT
	// among them while stdin is a console: raw mode (ENABLE_PROCESSED_INPUT off)
	// delivers it to the child as a byte, which is what an interactive CLI
	// expects. Window close is only a best-effort chance to restore the console:
	// while a SIGTERM Notify is registered Go's console handler blocks instead of
	// returning, which buys this goroutine time to run, but the real bound is
	// Windows' own close timeout (about 5 s), after which it terminates the
	// process whether or not the restore has run.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	// cancel lets the output pump end the session when stdout breaks.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	label := providerName
	if len(extraArgs) > 0 {
		label += " " + strings.Join(extraArgs, " ")
	}
	fmt.Fprintf(out, localizedText().continueStartedFmt, label, msg)

	cwd, _ := os.Getwd()
	lg.logf("started: provider=%s args=%v cwd=%s continue=%q", providerName, extraArgs, cwd, msg)

	stdinConsole := term.IsTerminal(int(os.Stdin.Fd()))
	stdoutConsole := term.IsTerminal(int(os.Stdout.Fd()))
	restoreConsole, err := setupConsole(stdinConsole, stdoutConsole)
	if err != nil {
		lg.logf("console setup failed: %v", err)
		return err
	}
	restoreOnce := false
	restore := func() {
		if !restoreOnce {
			restoreOnce = true
			restoreConsole()
		}
	}
	defer restore() // safety net for early returns; the normal path restores last, below.

	cols, rows := consoleSize(stdoutConsole)
	cmd := exec.Command(providerName, extraArgs...) // plain CLI; the user drives it
	sess, err := terminal.StartWithSize(ctx, cmd, cols, rows)
	if err != nil {
		lg.logf("failed to start %s: %v", providerName, err)
		return fmt.Errorf("starting %s: %w", providerName, err)
	}
	defer func() { _ = sess.Close() }()
	lg.logf("child: path=%s pid=%d size=%dx%d", cmd.Path, sess.Pid(), cols, rows)

	// All synthetic keystrokes (continue message, question confirmation) and the
	// user's own typing go through one sessionInjector so they cannot interleave.
	inject := &sessionInjector{w: sess}
	var questionConfirmer *claudeLimitQuestionConfirmer
	if providerName == "claude" {
		questionConfirmer = &claudeLimitQuestionConfirmer{inject: inject, lg: lg}
	}
	detector := &limitDetector{sig: &limitSignal{}, lg: lg, claudeQuestion: questionConfirmer, scanEvery: proxyScanThrottle}

	stdinStop := make(chan struct{})
	go func() {
		if err := pumpStdin(stdinStop, inject, os.Stdin, stdinConsole); err != nil {
			lg.logf("input pump ended: %v", err)
		}
	}()

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		if _, err := io.Copy(io.MultiWriter(os.Stdout, detector), sess); err != nil {
			// Nobody is draining the child's output any more, so it would block on
			// its next write; end the session instead of leaving it wedged.
			lg.logf("output pump failed: %v", err)
			cancel()
		}
	}()

	resizeStop, resizeDone := make(chan struct{}), make(chan struct{})
	if stdoutConsole {
		// Started even when the initial size lookup failed (cols == 0): the next
		// successful poll then sizes the session.
		go func() {
			defer close(resizeDone)
			watchResize(resizeStop, proxyResizePoll, cols, rows, func() (int, int, error) {
				return term.GetSize(int(os.Stdout.Fd()))
			}, sess.Resize, lg.logf)
		}()
	} else {
		close(resizeDone)
	}

	var selfResumed func(context.Context) bool
	if providerName == "claude" {
		selfResumed = func(ctx context.Context) bool {
			pid := claudeProcessPID(cmd.Path, sess.Pid(), listProcesses, lg)
			return claudeSelfResumed(ctx, detector, pid, lg)
		}
	}
	go watchAndContinue(ctx, p, inject, msg, cfg, detector, lg, selfResumed)

	err = sess.Wait()

	// Shutdown order matters: let the output pump flush what the child printed
	// last, stop the resize watcher, close the session (which kills stragglers and
	// unblocks the pump), wait for the pump to actually return, and only then
	// hand the console modes back — restoring them earlier would let the tail of
	// the child's VT output hit a console that no longer interprets it.
	waitClosed(outDone, proxyDrainTimeout)
	close(resizeStop)
	waitClosed(resizeDone, proxyStopTimeout)
	_ = sess.Close()
	// A console write that is already in progress cannot be interrupted, so this
	// wait is bounded. If it expires the console is restored anyway: leaving the
	// user's console raw with VT processing forced on is worse than the chance of
	// a few trailing VT bytes of the child's output landing after the restore,
	// which is accepted.
	if !waitClosed(outDone, proxyStopTimeout) {
		lg.logf("output pump still running at shutdown")
	}
	close(stdinStop)
	restore()
	lg.logf("exit: %v", err)
	return err
}

// waitClosed reports whether ch closed within d.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// setupConsole prepares the real console for passing a TUI through: VT output
// processing on stdout and raw input on stdin. Each is skipped when that stream
// is redirected, since there is no console mode to change. It fails, rather than
// carrying on half-configured, when a console cannot be switched: without raw
// input Ctrl+C would signal this process instead of reaching the child. The
// returned func restores the original modes.
func setupConsole(stdinConsole, stdoutConsole bool) (restore func(), err error) {
	var restores []func()
	restore = func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
	}
	if stdoutConsole {
		r, err := enableVT(windows.Handle(os.Stdout.Fd()))
		if err != nil {
			return nil, fmt.Errorf("this console does not support virtual terminal output, which limitping continue needs: %w", err)
		}
		restores = append(restores, r)
	}
	if stdinConsole {
		fd := int(os.Stdin.Fd())
		old, err := term.MakeRaw(fd)
		if err != nil {
			restore()
			return nil, fmt.Errorf("switching the console to raw input: %w", err)
		}
		restores = append(restores, func() { _ = term.Restore(fd, old) })
	}
	return restore, nil
}

// consoleSize is the visible size of the console behind stdout, or 0, 0 when
// stdout is not a console (or reports a nonsensical size), which makes the
// session start at the backend's default size.
func consoleSize(stdoutConsole bool) (cols, rows int) {
	if !stdoutConsole {
		return 0, 0
	}
	c, r, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || c < 1 || r < 1 || c > terminal.MaxTerminalDim || r > terminal.MaxTerminalDim {
		return 0, 0
	}
	return c, r
}

// enableVT turns on VT sequence processing for console output handle h and
// returns a func that restores the previous mode.
func enableVT(h windows.Handle) (restore func(), err error) {
	return enableVTMode(h, windows.GetConsoleMode, windows.SetConsoleMode)
}

// enableVTMode is enableVT over the console mode calls, as a seam for tests.
func enableVTMode(h windows.Handle, get func(windows.Handle, *uint32) error, set func(windows.Handle, uint32) error) (func(), error) {
	var old uint32
	if err := get(h, &old); err != nil {
		return nil, fmt.Errorf("GetConsoleMode: %w", err)
	}
	want := old | windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	// DISABLE_NEWLINE_AUTO_RETURN is unknown to older consoles, which reject it;
	// the child's output works without it, so retry without.
	if err := set(h, want|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		if err := set(h, want); err != nil {
			return nil, fmt.Errorf("SetConsoleMode: %w", err)
		}
	}
	return func() { _ = set(h, old) }, nil
}

// watchResize polls size every interval and calls resize when it differs from
// the last size the pseudoconsole was given (cols x rows to begin with). A size
// that is unreadable, empty (a minimized window) or that resize rejected is not
// recorded, so the next tick tries again. It returns when done closes or when
// resize reports os.ErrClosed. A failure other than that is reported through logf
// (when set) once, again only if its message changes or after a success, so a
// persistent error cannot flood the log at the poll rate.
func watchResize(done <-chan struct{}, interval time.Duration, cols, rows int, size func() (int, int, error), resize func(cols, rows int) error, logf func(format string, args ...any)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	lastErr := ""
	report := func(what string, err error) {
		if msg := what + ": " + err.Error(); msg != lastErr {
			lastErr = msg
			if logf != nil {
				logf("resize watcher: %s", msg)
			}
		}
	}
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		c, r, err := size()
		if err != nil {
			report("reading the console size", err)
			continue
		}
		if c < 1 || r < 1 || (c == cols && r == rows) {
			continue
		}
		if err := resize(c, r); err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			report(fmt.Sprintf("resizing to %dx%d", c, r), err)
			continue
		}
		lastErr = ""
		cols, rows = c, r
	}
}

// pumpStdin copies the user's input to dst until src or dst fails or done is
// closed. On a console, Ctrl+Z reads as EOF (a zero-byte read) instead of a
// byte, and an io.Copy that stopped there would leave the user unable to type
// into the session for good; so on a console an EOF only restarts the copy, with
// a growing delay while the EOFs come back empty so that a permanently dead
// stdin cannot spin. Ctrl+Z itself is therefore not forwarded to the child.
// Anything that is not a console (a pipe, a file) ends at its first EOF.
//
// The read is a blocking ReadConsole call that this does not interrupt
// (CancelSynchronousIo could, but the pump only needs to stop with the process);
// done is checked between reads.
func pumpStdin(done <-chan struct{}, dst io.Writer, src io.Reader, console bool) error {
	delay := stdinRetryMin
	for {
		n, err := io.Copy(dst, src)
		if err != nil || !console {
			return err
		}
		if n > 0 {
			delay = stdinRetryMin
		} else if delay *= 2; delay > stdinRetryMax {
			delay = stdinRetryMax
		}
		select {
		case <-done:
			return nil
		case <-time.After(delay):
		}
	}
}

// procEntry is one row of a process snapshot.
type procEntry struct {
	pid, ppid int
	exe       string
}

// listProcesses snapshots the running processes.
func listProcesses() ([]procEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return nil, err
	}
	var procs []procEntry
	for {
		procs = append(procs, procEntry{int(pe.ProcessID), int(pe.ParentProcessID), windows.UTF16ToString(pe.ExeFile[:])})
		if windows.Process32Next(snap, &pe) != nil {
			return procs, nil // ERROR_NO_MORE_FILES ends the walk
		}
	}
}

// claudeProcessPID is the pid to look for in `claude agents --json`, or -1 when
// it cannot be told (which matches no session). A native claude.exe is the
// session's own process. An npm install is a .cmd shim, so the session's pid is
// the cmd.exe interpreter and Claude Code itself is its child; that is resolved
// only for the one shape npm produces, and anything else is -1: a wrong pid that
// reports busy would suppress a continue that is needed, whereas -1 only risks a
// duplicate continue, the long-standing behaviour for a Claude Code that does
// not list its sessions.
func claudeProcessPID(cmdPath string, sessPid int, list func() ([]procEntry, error), lg *proxyLogger) int {
	switch strings.ToLower(filepath.Ext(cmdPath)) {
	case ".cmd", ".bat":
	default:
		return sessPid
	}
	procs, err := list()
	if err != nil {
		lg.logf("shim pid: listing processes failed: %v", err)
		return -1
	}
	pid := shimChildPID(procs, sessPid)
	if pid == 0 {
		lg.logf("shim pid: could not identify Claude Code under %s (pid %d); not using pid matching", cmdPath, sessPid)
		return -1
	}
	lg.logf("shim pid: Claude Code is pid %d under the %s shim (pid %d)", pid, filepath.Base(cmdPath), sessPid)
	return pid
}

// shimChildPID returns the pid of the single node.exe or claude.exe that is a
// direct child of root, when root is itself cmd.exe; otherwise 0.
func shimChildPID(procs []procEntry, root int) int {
	rootIsCmd, found := false, 0
	for _, p := range procs {
		if p.pid == root && strings.EqualFold(p.exe, "cmd.exe") {
			rootIsCmd = true
		}
		if p.ppid != root || !(strings.EqualFold(p.exe, "node.exe") || strings.EqualFold(p.exe, "claude.exe")) {
			continue
		}
		if found != 0 {
			return 0 // more than one candidate: ambiguous
		}
		found = p.pid
	}
	if !rootIsCmd {
		return 0
	}
	return found
}
