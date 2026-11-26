//go:build windows

package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// childPIDs lists the direct children of parent from a Toolhelp snapshot.
func childPIDs(t *testing.T, parent uint32) []uint32 {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var pids []uint32
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ParentProcessID == parent {
			pids = append(pids, pe.ProcessID)
		}
	}
	return pids
}

// waitChild polls until parent has a child process and returns its pid.
func waitChild(t *testing.T, parent uint32) uint32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if pids := childPIDs(t, parent); len(pids) > 0 {
			return pids[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d never started a child", parent)
	return 0
}

// processGone reports whether pid no longer runs (or never had a live handle).
func processGone(pid uint32) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == windows.WAIT_OBJECT_0
}

// writeSleepShim writes a .cmd that blocks ~60s in a grandchild (ping), ignoring stdin.
func writeSleepShim(t *testing.T, name, prefix string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("@echo off\r\n"+prefix+"ping -n 60 127.0.0.1 >nul\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKillProcessTreeReachesGrandchild(t *testing.T) {
	shim := writeSleepShim(t, "shim.cmd", "")
	cmd := exec.Command(shim)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	grandchild := waitChild(t, uint32(cmd.Process.Pid))
	t.Cleanup(func() {
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, grandchild); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
	})

	if err := killProcessTree(cmd.Process); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child still running")
	}
	for deadline := time.Now().Add(5 * time.Second); !processGone(grandchild); {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived killProcessTree", grandchild)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// answers initialize (id 0) and the request (id 1), then ignores stdin EOF.
const appServerReplies = `echo {"id":0,"result":{}}` + "\r\n" + `echo {"id":1,"result":{"ok":true}}` + "\r\n"

func useFakeCodex(t *testing.T, shim string) {
	t.Helper()
	old := codexBinary
	codexBinary = shim
	t.Cleanup(func() { codexBinary = old })
}

// The server never exits on stdin EOF: the deferred cleanup must still return
// (it used to rely on SIGTERM, which Windows does not support).
func TestRunCodexAppServerCleanupDoesNotHang(t *testing.T) {
	useFakeCodex(t, writeSleepShim(t, "codex.cmd", appServerReplies))

	type out struct {
		err error
	}
	res := make(chan out, 1)
	go func() {
		_, err := runCodexAppServer(context.Background(), "m", nil)
		res <- out{err}
	}()
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("runCodexAppServer: %v", r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runCodexAppServer cleanup hung on a server that ignores stdin EOF")
	}
}

// A silent server: ctx cancellation must return promptly instead of waiting for it.
func TestRunCodexAppServerCancelReturns(t *testing.T) {
	useFakeCodex(t, writeSleepShim(t, "codex.cmd", ""))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res := make(chan error, 1)
	go func() {
		_, err := runCodexAppServer(ctx, "m", nil)
		res <- err
	}()
	select {
	case err := <-res:
		if err == nil {
			t.Fatal("expected an error from a silent server")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runCodexAppServer did not return after ctx cancellation")
	}
}

// os/exec starts a bare "codex" that is only a .cmd on PATH, so app-server needs
// no PrepareCommand (its single argument has nothing for cmd.exe to misread).
func TestRunCodexAppServerFindsCmdShimOnPath(t *testing.T) {
	dir := t.TempDir()
	body := "@echo off\r\n" + appServerReplies + "ping -n 60 127.0.0.1 >nul\r\n"
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
