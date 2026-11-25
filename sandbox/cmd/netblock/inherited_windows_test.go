//go:build windows

package main

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"github.com/birdtable/go-proc/sandbox"
)

// startedWord, after the test binary's own flags and --, makes the test below write the file named
// after it and return: the trace a program Run or RunHolding started would leave, had it waited.
const startedWord = "netblock-started"

// TestWhatTheCallerHoldsInheritableAtRunNeverReachesTheProgram: on Windows the caller holds a file
// by a handle made inheritable (bInheritHandle true), which GetHandleInformation says it is, the
// control; then Run and RunHolding, each naming the test binary to run this test as a program that
// leaves a file behind, refuse with ErrNoBlock and ErrNoSystem, and no file is left: no program was
// started, so none could inherit the handle.
//
// What it costs when this fails: on Windows, a program is started for a caller that asked for the
// block, and inherits every handle the caller made inheritable, a socket among them.
func TestWhatTheCallerHoldsInheritableAtRunNeverReachesTheProgram(t *testing.T) {
	if a := flag.Args(); len(a) == 2 && a[0] == startedWord {
		if err := os.WriteFile(a[1], []byte("started"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	planted := filepath.Join(dir, "held")
	if err := os.WriteFile(planted, []byte("planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(planted)
	if err != nil {
		t.Fatal(err)
	}
	sa := &syscall.SecurityAttributes{InheritHandle: 1}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE, sa, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(h) })
	// Package syscall has SetHandleInformation and not GetHandleInformation, so kernel32's, a
	// system DLL Windows loads from its own folder, is called by name.
	var flags uint32
	got, _, callErr := syscall.NewLazyDLL("kernel32.dll").NewProc("GetHandleInformation").Call(uintptr(h), uintptr(unsafe.Pointer(&flags))) //nolint:gosec // G103: the call writes the handle's flags by their address
	if got == 0 || flags&syscall.HANDLE_FLAG_INHERIT == 0 {
		t.Fatalf("the control: the handle's flags are %#x (call %d, %v), want HANDLE_FLAG_INHERIT", flags, got, callErr)
	}
	args := func(trace string) []string {
		return []string{"-test.run=^TestWhatTheCallerHoldsInheritableAtRunNeverReachesTheProgram$", "--", startedWord, trace}
	}
	ran := 0
	for claim, call := range map[string]func(trace string) error{
		"Run": func(trace string) error { return sandbox.Run(self, args(trace), nil) },
		"RunHolding": func(trace string) error {
			return sandbox.RunHolding(sandbox.Folders{ReadWrite: []string{dir}}, self, args(trace), nil)
		},
	} {
		ran++
		trace := filepath.Join(dir, claim)
		if err := call(trace); !errors.Is(err, sandbox.ErrNoBlock) || !errors.Is(err, sandbox.ErrNoSystem) {
			t.Errorf("%s, holding an inheritable handle: %v, want ErrNoBlock and ErrNoSystem", claim, err)
		}
		if _, err := os.Stat(trace); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s, holding an inheritable handle: a program was started: %v", claim, err)
		}
	}
	if ran < 2 {
		t.Fatalf("ran %d calls", ran)
	}
}
