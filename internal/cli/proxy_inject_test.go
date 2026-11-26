package cli

import (
	"sync"
	"testing"
	"time"
)

// recordingWriter signals on its first write, so a test can act exactly while the
// injector is inside its settle pause instead of guessing at timing.
type recordingWriter struct {
	mu     sync.Mutex
	writes []string
	first  chan struct{}
	once   sync.Once
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	w.once.Do(func() { close(w.first) })
	return len(p), nil
}

// The user's own keystrokes must not land between an injected message and its
// Enter, or the submitted line would be corrupted.
func TestSessionInjectorSerializesUserInput(t *testing.T) {
	w := &recordingWriter{first: make(chan struct{})}
	inj := &sessionInjector{w: w}

	injected := make(chan struct{})
	go func() {
		defer close(injected)
		inj.typeAndSubmit("MSG", 150*time.Millisecond)
	}()
	select {
	case <-w.first: // "MSG" is written; the injector now sits in its settle pause
	case <-time.After(5 * time.Second):
		t.Fatal("the injector never wrote its message")
	}
	if _, err := inj.Write([]byte("x")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	<-injected

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
