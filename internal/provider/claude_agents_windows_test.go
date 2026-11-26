//go:build windows

package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An npm claude.cmd shim whose descendant outlives it keeps the stdout pipe open;
// the session listing must still return promptly with what was printed, instead
// of stalling the self-resume check past its grace period.
func TestClaudeAgentsOutputDoesNotWaitForPipeHolder(t *testing.T) {
	dir := t.TempDir()
	shim := "@echo off\r\necho []\r\nstart \"\" /b powershell -NoProfile -Command \"Start-Sleep 10\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "claude.cmd"), []byte(shim), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	out, err := claudeAgentsOutput(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("claudeAgentsOutput = %v, want the output the shim printed", err)
	}
	if !strings.Contains(string(out), "[]") {
		t.Fatalf("output = %q, want the shim's \"[]\"", out)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("took %v: the call waited on the pipe held by a descendant", elapsed)
	}
}
