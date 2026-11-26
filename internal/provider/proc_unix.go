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

// holdProcess is a no-op on Unix: os.Process.Signal/Kill already refuse to act
// on a process that Wait has reaped, so a pid cannot be reused under them.
func holdProcess(*os.Process) (release func()) { return func() {} }
