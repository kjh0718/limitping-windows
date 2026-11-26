//go:build !windows

package provider

import (
	"os"
	"syscall"
)

// terminateProcess asks p to exit.
func terminateProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Signal(syscall.SIGTERM)
}

// killProcessTree force-kills p; the same as os/exec's default Cancel.
func killProcessTree(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}

// procHold keeps a started child's pid from being reused while a Cancel may
// still run. Unix needs nothing: os.Process.Signal/Kill refuse to act on a
// process that Wait has reaped.
type procHold struct{}

func (*procHold) acquire(*os.Process) {}
func (*procHold) release()            {}

// terminate and kill are what a Cancel calls: SIGTERM and the default Kill.
func (*procHold) terminate(p *os.Process) error { return terminateProcess(p) }
func (*procHold) kill(p *os.Process) error      { return killProcessTree(p) }
