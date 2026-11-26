//go:build !windows

package terminal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
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

// After Close the master descriptor is released (and may be reused), so Resize
// must report os.ErrClosed as the Session contract says, not EBADF.
func TestSessionResizeAfterCloseIsErrClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := Start(ctx, exec.Command("sh", "-c", "sleep 5"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Kill()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sess.Resize(100, 40); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Resize after Close = %v, want os.ErrClosed", err)
	}
}

func TestSessionConcurrentResizeAndClose(t *testing.T) {
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		sess, err := Start(ctx, exec.Command("sh", "-c", "sleep 5"))
		if err != nil {
			cancel()
			t.Fatalf("Start: %v", err)
		}
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
		time.Sleep(5 * time.Millisecond)
		_ = sess.Close()
		close(stop)
		wg.Wait()
		if err := sess.Resize(80, 24); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("iteration %d: Resize after Close = %v, want os.ErrClosed", i, err)
		}
		_ = sess.Kill()
		cancel()
	}
}
