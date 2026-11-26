//go:build windows

package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// terminateProcess ends p and its descendants. Windows has no SIGTERM for a
// plain child (Process.Signal only supports Kill), so this is a forced tree kill.
func terminateProcess(p *os.Process) error { return killProcessTree(p) }

// killProcessTree force-kills p and everything it started. An npm-installed CLI
// is codex.cmd -> cmd.exe -> node -> codex.exe, so killing only the direct child
// leaves the real work running with the output pipes still open.
//
// The caller must keep a procHold's handle open while a Cancel can still run:
// taskkill takes a bare PID, and without an open handle the PID of an exited
// child may already belong to an unrelated process.
//
// ponytail: taskkill /T finds descendants by parent pid, so a grandchild whose
// parent already died is missed; assign the child to a job object at start if
// that ever matters.
func killProcessTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	taskkill := "taskkill"
	if root := os.Getenv("SystemRoot"); root != "" {
		taskkill = filepath.Join(root, "System32", "taskkill.exe")
	}
	cmd := exec.Command(taskkill, "/T", "/F", "/PID", strconv.Itoa(p.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if cmd.Run() == nil {
		return nil
	}
	return p.Kill() // taskkill unavailable or the pid is gone; os.ErrProcessDone if finished
}

// openProcess is a seam for tests of the not-held fallback.
var openProcess = windows.OpenProcess

// procHold opens a second handle to a started child and keeps it until release.
// Go's own handle is closed as soon as Wait observes the exit, which can be
// before Cmd.Wait has finished reading the context result, so a late Cancel could
// otherwise run taskkill against a PID the OS has since handed to someone else. A
// process id stays reserved while any handle to it is open, so holding one from
// Start until Cmd.Wait returns makes killing by PID safe for the whole lifetime.
// The zero value is ready: use it in the Cancel closure, then acquire right after
// Start (before Wait: until then Go's handle is still open and the PID cannot
// have been reused), and release after Wait.
//
// If the handle cannot be opened nothing reserves the PID, so terminate and kill
// fall back to Process.Kill, which acts through Go's own handle and cannot hit a
// stranger (it ends the direct child only).
//
// The mutex orders a Cancel that fires between Start and acquire: kill holds it
// for the whole kill, and acquire needs it, so the caller's following Wait (which
// is what closes Go's handle) cannot start until that kill has finished. Either
// acquire ran first and kill sees its outcome, or kill ran first with Go's handle
// still open. release runs after Wait, and Cmd.Wait does not return before a
// running Cancel has, so release never waits on a live kill.
type procHold struct {
	mu     sync.Mutex
	h      windows.Handle
	failed bool
}

func (h *procHold) acquire(p *os.Process) {
	if p == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	hd, err := openProcess(windows.SYNCHRONIZE, false, uint32(p.Pid))
	if err != nil {
		h.failed = true
		return
	}
	h.h = hd
}

func (h *procHold) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.h != 0 {
		_ = windows.CloseHandle(h.h)
		h.h = 0
	}
}

func (h *procHold) terminate(p *os.Process) error { return h.kill(p) }

func (h *procHold) kill(p *os.Process) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed {
		if p == nil {
			return nil
		}
		return p.Kill()
	}
	return killProcessTree(p)
}
