//go:build windows

package terminal

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestBatchArgsSurviveCRTParsing covers the two parses an npm shim's arguments go
// through: cmd.exe's, then the CRT's when the shim forwards %* to node. Node is
// the real consumer and the only one worth asserting against: Go's own command
// line parser treats a doubled quote differently from the UCRT that node's argv
// comes from, so a Go helper would check the wrong rules. The backslash cases are
// the ones a quotes-only escape gets wrong: a trailing backslash swallows the
// closing quote, and a backslash before a quote is consumed as its escape.
func TestBatchArgsSurviveCRTParsing(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; it is the CRT program npm shims forward %* to")
	}
	quotedNode, err := quoteBatchArg(node)
	if err != nil {
		t.Skipf("node path cannot be passed through a batch file: %v", err)
	}
	shim := writeShim(t, "dir with space", "limitping-crt.cmd",
		"@echo off\r\n"+quotedNode+` -e "console.log('__ARGV__'+JSON.stringify(process.argv.slice(1))+'__END__')" %*`+"\r\n")

	for _, args := range [][]string{
		{`C:\foo\`, "next"},
		{`end\`},
		{`a\"b`, "c"},
		{`a\\"b`, "c"},
		{`\\srv\share`, `a\\"&echo X`},
		{`a"b`, "x&echo PWN"},
		{"hello world", `say "hi"`, "x & y | z < > ^ ( ) !"},
		{"plain"},
	} {
		cmd := exec.Command(shim, args...)
		if err := PrepareCommand(cmd); err != nil {
			t.Fatalf("PrepareCommand(%q) = %v", args, err)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Run(%q) = %v: %s", args, err, out)
		}
		s := string(out)
		start := strings.Index(s, "__ARGV__")
		end := strings.Index(s, "__END__")
		if start < 0 || end < start {
			t.Fatalf("args %q: no node output in %q", args, s)
		}
		var got []string
		if err := json.Unmarshal([]byte(s[start+len("__ARGV__"):end]), &got); err != nil {
			t.Fatalf("args %q: decoding %q: %v", args, s, err)
		}
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") || len(got) != len(args) {
			t.Fatalf("args %q arrived as %q", args, got)
		}
		// Anything outside node's own report is output of something else that
		// ran, e.g. an injected `echo PWN`.
		if rest := s[:start] + s[end+len("__END__"):]; strings.Contains(rest, "PWN") {
			t.Fatalf("args %q: an argument ran as a command: %q", args, s)
		}
	}
}
