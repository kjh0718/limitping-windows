package provider

import "testing"

func TestProcessHelpersIgnoreNil(t *testing.T) {
	if err := terminateProcess(nil); err != nil {
		t.Fatalf("terminateProcess(nil) = %v", err)
	}
	if err := killProcessTree(nil); err != nil {
		t.Fatalf("killProcessTree(nil) = %v", err)
	}
}

func TestHoldProcessNilIsNoop(t *testing.T) {
	var h procHold
	h.acquire(nil)
	h.release()
	if err := h.kill(nil); err != nil {
		t.Fatalf("kill(nil) = %v", err)
	}
}
