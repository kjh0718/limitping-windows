//go:build windows

package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// The caller must keep holdProcess's handle open while a Cancel can still run:
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

// holdProcess opens a second handle to p's process and returns the function that
// closes it. Go's own handle is closed as soon as Wait observes the exit, which
// can be before Cmd.Wait has finished reading the context result, so a late
// Cancel could otherwise run taskkill against a PID the OS has since handed to
// someone else. A process id stays reserved while any handle to it is open, so
// holding one from Start until Cmd.Wait returns makes killing by PID safe for the
// whole lifetime. Call it right after Start, before Wait: until then Go's handle
// is still open and the PID cannot have been reused.
func holdProcess(p *os.Process) (release func()) {
	if p == nil {
		return func() {}
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(p.Pid))
	if err != nil {
		return func() {}
	}
	return func() { _ = windows.CloseHandle(h) }
}
