// Package terminal abstracts launching a child process attached to a
// pseudo-terminal, so provider triggers can drive an interactive CLI (Claude
// Code, Codex) without depending on a specific PTY backend. Unix/macOS uses a
// real PTY (github.com/creack/pty); Windows uses a ConPTY pseudoconsole, with
// the child and its descendants held in a job object so the whole tree can be
// torn down.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
)

// MaxTerminalDim is the largest column or row count Start/Resize accept. It is
// the limit of a ConPTY COORD (int16), applied on every OS so both backends
// reject the same sizes.
const MaxTerminalDim = math.MaxInt16

// Session is a child process running under a pseudo-terminal. Read and Write
// are the PTY master side — Read drains the child's output, Write feeds its
// input. Close tears the PTY down, Wait blocks until the child exits, and Kill
// force-terminates it. The abstraction owns the whole process lifecycle (not
// just the I/O stream) because a PTY backend may spawn the child itself rather
// than through os/exec, so Wait and Kill cannot be assumed to come from an
// externally held *exec.Cmd.
type Session interface {
	io.Reader
	io.Writer
	io.Closer
	// Wait blocks until the child exits, returning its exit error (nil on a
	// clean exit), matching (*exec.Cmd).Wait semantics.
	Wait() error
	// Kill force-terminates the child. It is safe to call when the process is
	// already gone.
	Kill() error
	// Resize sets the pseudo-terminal's size in character cells. Both
	// dimensions must be in 1..MaxTerminalDim. After the session has been closed
	// (or, on Windows, once the child has exited) it returns os.ErrClosed.
	Resize(cols, rows int) error
	// Pid is the operating-system process id of the child that Start created
	// (for a .cmd/.bat target on Windows that is the cmd.exe interpreter, not
	// the script's own descendants).
	Pid() int
}

// Start launches cmd attached to a new pseudo-terminal and returns a Session
// that owns the child's lifecycle. Cancelling ctx terminates the child, whether
// cmd came from exec.Command or exec.CommandContext. On Unix pty.Start runs
// cmd.Start, so a CommandContext's own Cancel applies as well; on Windows the
// process is created by hand, cmd.Cancel and cmd.WaitDelay are never consulted,
// and ctx alone kills the whole job. cmd must not have been started.
//
// Only Path, Args, Dir and Env of cmd are honoured (a nil Env inherits this
// process's environment, as for os/exec). The PTY is the child's stdio and
// session, so Stdin, Stdout, Stderr, ExtraFiles and SysProcAttr must be unset;
// Start rejects them on every OS rather than let one backend silently ignore a
// field the other honours. A LookPath failure recorded in cmd.Err is returned.
// It returns an error and a nil Session if ctx is already done (the child is
// never started) or if the terminal or the child cannot be created.
func Start(ctx context.Context, cmd *exec.Cmd) (Session, error) {
	return StartWithSize(ctx, cmd, 0, 0)
}

// StartWithSize is Start with an initial terminal size, so a TUI lays itself out
// at the real size from its first frame instead of racing a later Resize. cols
// and rows of 0 and 0 keep the platform default (the OS pty default on Unix,
// 120x30 on Windows); otherwise both must be in 1..MaxTerminalDim.
func StartWithSize(ctx context.Context, cmd *exec.Cmd, cols, rows int) (Session, error) {
	if cols != 0 || rows != 0 {
		if err := validateSize(cols, rows); err != nil {
			return nil, err
		}
	}
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	if cmd.Process != nil {
		return nil, errors.New("terminal: cmd has already been started")
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil ||
		len(cmd.ExtraFiles) != 0 || cmd.SysProcAttr != nil {
		return nil, errors.New("terminal: Stdin, Stdout, Stderr, ExtraFiles and SysProcAttr are not supported; the pseudo-terminal is the child's stdio")
	}
	// A ctx that is already done must not run the child at all, as with
	// exec.CommandContext; checked before any terminal resource is allocated.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return start(ctx, cmd, cols, rows)
}

// validateSize rejects anything but a positive, in-range cols x rows.
func validateSize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > MaxTerminalDim || rows > MaxTerminalDim {
		return fmt.Errorf("terminal: invalid size %dx%d (each dimension must be 1..%d)", cols, rows, MaxTerminalDim)
	}
	return nil
}

// PrepareCommand adapts cmd for plain os/exec use (Run/Start, not Start above) so
// it launches correctly on the platform. On Windows a .cmd/.bat target (how
// npm-installed CLIs ship) is rewritten to run through COMSPEC with cmd.exe-safe
// quoting, and arguments that cmd.exe cannot carry intact (NUL, newline, %) are
// rejected with an error. On Unix it does nothing.
func PrepareCommand(cmd *exec.Cmd) error { return prepareCommand(cmd) }
