package proc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/birdtable/go-proc/internal/homevars"
)

// FileName gives .exe on windows alone, for any id, on any host.
//
// What it costs when this fails: a program written under a name without .exe does not run on
// Windows, or one written with it is not found by its name on Unix.
func TestFileNameIsTheExeRuleOfEachSystem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ goos, id, want string }{
		{"windows", "tool", "tool.exe"},
		{"windows", "bin/tool", "bin/tool.exe"},
		{"windows", "tool.exe", "tool.exe.exe"},
		{"linux", "tool", "tool"},
		{"darwin", "tool", "tool"},
		{"freebsd", "tool.exe", "tool.exe"},
		{"Windows", "tool", "tool"},
	} {
		if got := FileName(tc.goos, tc.id); got != tc.want {
			t.Errorf("FileName(%q, %q) is %q, want %q", tc.goos, tc.id, got, tc.want)
		}
	}
}

// LocalFileName gives what FileName gives for the system the test runs on, as runtime.GOOS names
// it, so the name it gives is this system's and no other's.
//
// What it costs when this fails: a caller that may not name the system writes or looks
// for a program by a name this system doesn't give it, with .exe missing on Windows or added on Unix.
func TestLocalFileNameIsTheNameOnThisSystem(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"tool", "bin/tool", "tool.exe"} {
		if got, want := LocalFileName(id), FileName(hostGOOS(), id); got != want {
			t.Errorf("LocalFileName(%q) is %q, and FileName(%q, %q) is %q", id, got, hostGOOS(), id, want)
		}
	}
}

// A program copied to FileName(homevars.Here, id) runs by that path here, on every system.
//
// What it costs when this fails: code that writes a program's file by FileName writes one this
// system does not start.
//
// The copy is written by writeExe, which no fork of this process starts within, so that no child a
// test beside this one starts holds it open for writing when it runs: on Linux that run fails
// `text file busy`, as it once did.
func TestAProgramWrittenByFileNameRunsHere(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), FileName(homevars.Here, "copied"))
	if err := writeExe(path, b); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "argv")
	c.Path = path
	r, err := Run(t.Context(), c)
	if err != nil || r.Code != 0 {
		t.Fatalf("the copy at %s: %v, code %d, stderr %q", path, err, r.Code, r.Stderr)
	}
}
