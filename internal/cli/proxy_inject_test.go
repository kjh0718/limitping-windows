package cli

import (
	"sync"
	"testing"
	"time"
)

type recordingWriter struct {
	mu     sync.Mutex
	writes []string
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

// The user's own keystrokes must not land between an injected message and its
// Enter, or the submitted line would be corrupted.
func TestSessionInjectorSerializesUserInput(t *testing.T) {
	w := &recordingWriter{}
	inj := &sessionInjector{w: w}

	injecting := make(chan struct{})
	go func() {
		close(injecting)
		inj.typeAndSubmit("MSG", 150*time.Millisecond)
	}()
	<-injecting
	time.Sleep(50 * time.Millisecond) // inside the settle pause
	if _, err := inj.Write([]byte("x")); err != nil {
		t.Fatalf("Write = %v", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	want := []string{"MSG", "\r", "x"}
	if len(w.writes) != len(want) {
		t.Fatalf("writes = %q, want %q", w.writes, want)
	}
	for i := range want {
		if w.writes[i] != want[i] {
			t.Fatalf("writes = %q, want %q: user input interleaved with the injection", w.writes, want)
		}
	}
}
