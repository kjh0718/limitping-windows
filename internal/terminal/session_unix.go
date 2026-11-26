//go:build !windows

package terminal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// unixSession wraps a creack/pty master file and the exec.Cmd running under it.
// This preserves the behavior the provider triggers relied on before the
// terminal seam was introduced: pty.Start attaches the PTY, and the master file
// is the child's stdin/stdout.
type unixSession struct {
	cmd  *exec.Cmd
	ptmx *os.File
	// waited is closed by Wait, ending the context watcher.
	waited     chan struct{}
	waitedOnce sync.Once

	// mu orders Resize against Close: Resize holds it across the ioctl, so it can
	// never run on a descriptor Close has released (and the OS may have reused).
	mu     sync.Mutex
	closed bool
}

func start(ctx context.Context, cmd *exec.Cmd, cols, rows int) (Session, error) {
	var (
		ptmx *os.File
		err  error
	)
	if cols > 0 {
		ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	} else {
		ptmx, err = pty.Start(cmd)
	}
	if err != nil {
		return nil, err
	}
	s := &unixSession{cmd: cmd, ptmx: ptmx, waited: make(chan struct{})}
	// A cmd from exec.CommandContext is already bound to its own ctx, but one from
	// exec.Command is not, and Start's contract is that ctx governs the child.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Kill()
		case <-s.waited:
		}
	}()
	return s, nil
}

// prepareCommand is a no-op: os/exec runs scripts through their shebang on Unix.
func prepareCommand(*exec.Cmd) error { return nil }

func (s *unixSession) Read(p []byte) (int, error)  { return s.ptmx.Read(p) }
func (s *unixSession) Write(p []byte) (int, error) { return s.ptmx.Write(p) }
func (s *unixSession) Pid() int                    { return s.cmd.Process.Pid }

func (s *unixSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.ptmx.Close()
}

func (s *unixSession) Resize(cols, rows int) error {
	if err := validateSize(cols, rows); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	return pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}
func (s *unixSession) Wait() error {
	err := s.cmd.Wait()
	s.waitedOnce.Do(func() { close(s.waited) })
	return err
}

// Kill force-terminates the child. Once Wait has reaped it the process handle is
// marked done and Process.Kill reports os.ErrProcessDone, which is the state Kill
// is asked to reach, not a failure to reach it — the Session contract makes Kill
// safe on an already-gone child, and the Windows backend behaves that way because
// terminating a job whose members have exited succeeds. Every other error is a
// real failure to terminate and is returned.
func (s *unixSession) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
