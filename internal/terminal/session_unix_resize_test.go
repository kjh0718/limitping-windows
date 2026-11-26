//go:build !windows

package terminal

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestSessionResizeSetsPtySize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := StartWithSize(ctx, exec.Command("sh", "-c", "sleep 5"), 100, 40)
	if err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}
	defer sess.Close()
	defer sess.Kill()

	ptmx := sess.(*unixSession).ptmx
	if rows, cols, err := pty.Getsize(ptmx); err != nil || cols != 100 || rows != 40 {
		t.Fatalf("initial size = %dx%d (err %v), want 100x40", cols, rows, err)
	}
	if err := sess.Resize(132, 43); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if rows, cols, err := pty.Getsize(ptmx); err != nil || cols != 132 || rows != 43 {
		t.Fatalf("size after Resize = %dx%d (err %v), want 132x43", cols, rows, err)
	}
	for _, c := range [][2]int{{0, 10}, {10, 0}, {-1, 5}, {MaxTerminalDim + 1, 5}} {
		if err := sess.Resize(c[0], c[1]); err == nil {
			t.Errorf("Resize(%d, %d) accepted an invalid size", c[0], c[1])
		}
	}
}

func TestSessionPid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command("sh", "-c", "sleep 5")
	sess, err := Start(ctx, cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()
	defer sess.Kill()
	if sess.Pid() <= 0 || sess.Pid() != cmd.Process.Pid {
		t.Fatalf("Pid = %d, want cmd.Process.Pid %d", sess.Pid(), cmd.Process.Pid)
	}
}
