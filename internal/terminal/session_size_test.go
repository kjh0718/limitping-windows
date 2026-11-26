package terminal

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Size validation sits in StartWithSize, ahead of the per-OS backends, so both
// OSes reject the same sizes and none of them launches the child first.
func TestStartWithSizeRejectsInvalidSizes(t *testing.T) {
	cases := []struct{ cols, rows int }{
		{0, 24}, {80, 0}, {-1, 24}, {80, -1}, {-5, -5},
		{MaxTerminalDim + 1, 24}, {80, MaxTerminalDim + 1},
	}
	for _, c := range cases {
		// A binary that cannot exist: should validation ever be skipped the error
		// is a LookPath one, and no child is ever started.
		sess, err := StartWithSize(context.Background(), exec.Command("limitping-no-such-binary-xyz"), c.cols, c.rows)
		if err == nil {
			_ = sess.Kill()
			_ = sess.Close()
			t.Errorf("StartWithSize(%d, %d) accepted an invalid size", c.cols, c.rows)
		} else if !strings.Contains(err.Error(), "invalid size") {
			t.Errorf("StartWithSize(%d, %d) = %v, want the size validation error", c.cols, c.rows, err)
		}
	}
}
