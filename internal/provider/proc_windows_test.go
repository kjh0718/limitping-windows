//go:build windows

package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

// ourChildren returns the pids of this test process's current children.
func ourChildren(t *testing.T) map[uint32]bool {
	_, parents := processes(t)
	set := map[uint32]bool{}
	for pid, parent := range parents {
		if parent == uint32(os.Getpid()) {
			set[pid] = true
		}
	}
	return set
}

// newPing holds a PING.EXE that descends from a child this test process started
// after before was taken. runCodexAppServer hides its Cmd, so the child (the
// shim's cmd.exe, or the fake server) is recognised as a new direct child of ours,
// and the grandchild by walking its parent chain up to it. An unrelated ping.exe anywhere else on the
// machine, or a leftover from an earlier test, is never picked.
func newPing(t *testing.T, before map[uint32]bool) windows.Handle {
	return holdFirst(t, func(names map[uint32]string, parents map[uint32]uint32) (uint32, bool) {
		for pid, name := range names {
			if name != "ping.exe" {
				continue
			}
			for anc, hops := parents[pid], 0; anc != 0 && hops < 8; anc, hops = parents[anc], hops+1 {
				if parents[anc] == uint32(os.Getpid()) && !before[anc] {
					return pid, true
				}
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

// procHold keeps the pid reserved after Go's own handle is released by Wait.
func TestProcHoldKeepsPidReservedAfterWait(t *testing.T) {
	cmd := exec.Command(os.Getenv("ComSpec"), "/c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := uint32(cmd.Process.Pid)
	var hold procHold
	hold.acquire(cmd.Process)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// The process is gone, yet its pid still opens: only a held handle does that.
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		t.Fatalf("pid %d was released although the hold had not: %v", pid, err)
	}
	_ = windows.CloseHandle(h)
	hold.release()
}

// Without a held handle nothing reserves the pid, so kill must not run taskkill
// by pid: it ends only the direct child, through Go's own handle.
func TestProcHoldNotHeldFallsBackToDirectKill(t *testing.T) {
	old := openProcess
	openProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, windows.ERROR_ACCESS_DENIED }
	t.Cleanup(func() { openProcess = old })

	shim := writeScript(t, "shim.cmd", "@echo off\nping -n 60 127.0.0.1 >nul\n")
	cmd := exec.Command(shim)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	grandchild := pingChildOf(t, uint32(cmd.Process.Pid))

	var hold procHold
	hold.acquire(cmd.Process)
	if err := hold.kill(cmd.Process); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("direct child still running")
	}
	if ev, _ := windows.WaitForSingleObject(grandchild, 500); ev == windows.WAIT_OBJECT_0 {
		t.Fatal("the grandchild died: the not-held path used a tree kill by pid")
	}
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
	before := ourChildren(t)

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

// fakeServerEnv makes the test binary itself act as a stuck `codex app-server`
// (see runFakeAppServer): a real program reading JSON lines is deterministic where
// a batch script's `set /p` on a pipe was not.
const fakeServerEnv = "LIMITPING_FAKE_APPSERVER_MARKER"

func init() {
	if marker := os.Getenv(fakeServerEnv); marker != "" {
		runFakeAppServer(marker)
		os.Exit(0)
	}
}

// runFakeAppServer answers initialize, reads `initialized` and the method call,
// starts a long-lived grandchild, records that the request arrived by creating
// marker, and then never answers or exits.
func runFakeAppServer(marker string) {
	in := bufio.NewScanner(os.Stdin)
	for i := 0; i < 3; i++ { // initialize, initialized, the method call
		if !in.Scan() {
			return
		}
		if i == 0 {
			fmt.Println(`{"id":0,"result":{}}`)
		}
	}
	ping := exec.Command("ping", "-n", "60", "127.0.0.1")
	if ping.Start() != nil {
		return
	}
	_ = os.WriteFile(marker, []byte("x"), 0o644)
	time.Sleep(time.Minute)
}

// The fake server (this test binary) reads the method call, starts a grandchild
// (proved by a marker file) and never answers. From then on the call is parked on
// a pending request, and the only way out is cancelling ctx, which must end the
// call and the whole tree.
func TestRunCodexAppServerCancelKillsTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "request-received")
	t.Setenv(fakeServerEnv, marker)
	useFakeCodex(t, os.Args[0])
	before := ourChildren(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() {
		_, err := runCodexAppServer(ctx, "m", nil)
		res <- err
	}()
	// The marker is written after the server read the method call, so from here
	// the call is parked on a pending request.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake server never received the request")
		}
	}
	grandchild := newPing(t, before)
	cancel()
	select {
	case err := <-res:
		// A failure before the request was sent would be errCodexAppServerUnavailable
		// (which lets the caller retry over HTTP); a cut-off pending request is not.
		if errors.Is(err, errCodexAppServerUnavailable) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want the pending request cut off by the kill", err)
		}
		if ctx.Err() != context.Canceled {
			t.Fatalf("ctx.Err() = %v, want Canceled", ctx.Err())
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
