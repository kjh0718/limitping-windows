package terminal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// The unsupported-field rejection lives in Start itself, ahead of the per-OS
// backends, so both OSes honour the same contract.
func TestStartRejectsUnsupportedCmdFields(t *testing.T) {
	cases := map[string]func(*exec.Cmd){
		"Stdin":       func(c *exec.Cmd) { c.Stdin = strings.NewReader("") },
		"Stdout":      func(c *exec.Cmd) { c.Stdout = &bytes.Buffer{} },
		"Stderr":      func(c *exec.Cmd) { c.Stderr = &bytes.Buffer{} },
		"ExtraFiles":  func(c *exec.Cmd) { c.ExtraFiles = []*os.File{os.Stdout} },
		"SysProcAttr": func(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{} },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			set(cmd)
			sess, err := Start(context.Background(), cmd)
			if err == nil {
				_ = sess.Kill()
				_ = sess.Close()
				t.Fatal("Start accepted an unsupported field")
			}
			if !strings.Contains(err.Error(), "not supported") {
				t.Fatalf("Start = %v, want the unsupported-field error", err)
			}
		})
	}
}

func TestStartReturnsLookPathError(t *testing.T) {
	_, err := Start(context.Background(), exec.Command("limitping-no-such-binary-xyz"))
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Start = %v, want exec.ErrNotFound from cmd.Err", err)
	}
}

func TestStartRejectsStartedCmd(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Process = &os.Process{}
	if _, err := Start(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "already been started") {
		t.Fatalf("Start = %v, want the already-started error", err)
	}
}
