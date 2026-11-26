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
// ponytail: taskkill /T finds descendants by parent pid, so a grandchild already
// re-parented after its parent died is missed; assign the child to a job object
// at start if that ever matters.
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
