package terminal

import (
	"context"
	"os"
	"os/exec"
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
		sess, err := StartWithSize(context.Background(), exec.Command(os.Args[0]), c.cols, c.rows)
		if err == nil {
			_ = sess.Kill()
			_ = sess.Close()
			t.Errorf("StartWithSize(%d, %d) accepted an invalid size", c.cols, c.rows)
		}
	}
}
