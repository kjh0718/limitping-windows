//go:build windows

package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processes returns pid -> lower-cased image name and pid -> parent pid.
func processes(t *testing.T) (names map[uint32]string, parents map[uint32]uint32) {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snap)
	names, parents = map[uint32]string{}, map[uint32]uint32{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		names[pe.ProcessID] = strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		parents[pe.ProcessID] = pe.ParentProcessID
	}
	return names, parents
}

// holdFirst polls until pick finds a process and returns a held handle to it.
// Everything below judges the process through this handle, never by PID, so a
// reused PID can neither fool a check nor get a stranger killed; the cleanup
// terminates it through the same handle if the test left it running.
func holdFirst(t *testing.T, pick func(names map[uint32]string, parents map[uint32]uint32) (uint32, bool)) windows.Handle {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		names, parents := processes(t)
		pid, ok := pick(names, parents)
		if !ok {
			continue
		}
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid); err == nil {
			t.Cleanup(func() {
				_ = windows.TerminateProcess(h, 1)
				_ = windows.CloseHandle(h)
			})
			return h
		}
	}
	t.Fatal("no PING.EXE grandchild appeared")
	return 0
}

// pingChildOf holds the PING.EXE that parent started; conhost.exe and others can
// be children too, so the first child is not necessarily the one.
func pingChildOf(t *testing.T, parent uint32) windows.Handle {
	return holdFirst(t, func(names map[uint32]string, parents map[uint32]uint32) (uint32, bool) {
		for pid, name := range names {
			if name == "ping.exe" && parents[pid] == parent {
				return pid, true
			}
		}
		return 0, false
	})
}

// pingPIDs returns the PING.EXE pids running now.
func pingPIDs(t *testing.T) map[uint32]bool {
	names, _ := processes(t)
	set := map[uint32]bool{}
	for pid, name := range names {
		if name == "ping.exe" {
			set[pid] = true
		}
	}
	return set
}

// newPing holds a PING.EXE that was not running in before. The app-server tests
// cannot see the direct child's pid (it lives inside runCodexAppServer), so the
// grandchild is found by being new.
func newPing(t *testing.T, before map[uint32]bool) windows.Handle {
	return holdFirst(t, func(names map[uint32]string, _ map[uint32]uint32) (uint32, bool) {
		for pid, name := range names {
			if name == "ping.exe" && !before[pid] {
				return pid, true
			}
		}
		return 0, false
	})
}

func requireGone(t *testing.T, h windows.Handle, what string) {
	t.Helper()
	if ev, _ := windows.WaitForSingleObject(h, 5000); ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("%s is still running", what)
	}
}

func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "\n", "\r\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKillProcessTreeReachesGrandchild(t *testing.T) {
	shim := writeScript(t, "shim.cmd", "@echo off\nping -n 60 127.0.0.1 >nul\n")
	cmd := exec.Command(shim)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	grandchild := pingChildOf(t, uint32(cmd.Process.Pid))
	if err := killProcessTree(cmd.Process); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child still running")
	}
	requireGone(t, grandchild, "the grandchild")
}

// holdProcess keeps the pid reserved after Go's own handle is released by Wait.
func TestHoldProcessKeepsPidReservedAfterWait(t *testing.T) {
	cmd := exec.Command(os.Getenv("ComSpec"), "/c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid)
	release := holdProcess(cmd.Process)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// The process is gone, yet its pid still opens: only a held handle does that.
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		t.Fatalf("pid %d was released although holdProcess had not: %v", pid, err)
	}
	_ = windows.CloseHandle(h)
	release()
}

// The fake codex answers the two RPCs.
const appServerReplies = `echo {"id":0,"result":{}}` + "\n" + `echo {"id":1,"result":{"ok":true}}` + "\n"

// delaySignal blocks n seconds without reading stdin and without spawning a
// PING.EXE (which the tests use to recognise the grandchild).
func delaySignal(n string) string { return "waitfor /t " + n + " limitpingnever >nul 2>&1\n" }

func useFakeCodex(t *testing.T, shim string) {
	t.Helper()
	old := codexBinary
	codexBinary = shim
	t.Cleanup(func() { codexBinary = old })
}

// The server spawns a grandchild, answers, then ignores stdin EOF: the deferred
// cleanup must return and must take the grandchild down with the server (a kill
// of the direct cmd.exe alone would leave it, and SIGTERM would hang).
func TestRunCodexAppServerCleanupKillsTree(t *testing.T) {
	useFakeCodex(t, writeScript(t, "codex.cmd",
		"@echo off\nstart /b ping -n 60 127.0.0.1 >nul\n"+delaySignal("3")+appServerReplies+delaySignal("60")))
	before := pingPIDs(t)

	res := make(chan error, 1)
	go func() {
		_, err := runCodexAppServer(context.Background(), "m", nil)
		res <- err
	}()
	// The server holds its answer for 3s, so the grandchild is there to be held.
	grandchild := newPing(t, before)
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("runCodexAppServer: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runCodexAppServer cleanup hung on a server that ignores stdin EOF")
	}
	requireGone(t, grandchild, "the app-server's grandchild")
}

// A silent server: cancelling ctx must return promptly and kill its tree.
func TestRunCodexAppServerCancelKillsTree(t *testing.T) {
	useFakeCodex(t, writeScript(t, "codex.cmd",
		"@echo off\nstart /b ping -n 60 127.0.0.1 >nul\n"+delaySignal("60")))
	before := pingPIDs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() {
		_, err := runCodexAppServer(ctx, "m", nil)
		res <- err
	}()
	grandchild := newPing(t, before) // the server really started
	cancel()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("expected an error from a silent server")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runCodexAppServer did not return after ctx cancellation")
	}
	requireGone(t, grandchild, "the app-server's grandchild")
}

// os/exec starts a bare "codex" that is only a .cmd on PATH, so app-server needs
// no PrepareCommand (its single argument has nothing for cmd.exe to misread).
func TestRunCodexAppServerFindsCmdShimOnPath(t *testing.T) {
	dir := t.TempDir()
	body := "@echo off\r\n" + strings.ReplaceAll(appServerReplies+delaySignal("60"), "\n", "\r\n")
	if err := os.WriteFile(filepath.Join(dir, "codex.cmd"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	useFakeCodex(t, "codex")

	done := make(chan error, 1)
	go func() {
		_, err := runCodexAppServer(context.Background(), "m", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runCodexAppServer via PATH shim: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("hung")
	}
}
