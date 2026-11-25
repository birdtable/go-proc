package proc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestARunInNeutralDirStartsThere: a run whose Dir is NeutralDir starts its program there, the root
// of a drive: / on Unix, and on Windows the system drive's root, whose volume is the system
// folder's. A run with an empty Dir starts in this process's folder, as os/exec has it, on every
// system. The Windows row is proved by a native run.
//
// What it costs when this fails: a caller that asks for a folder that is no project's gets one
// Windows refuses, or one that is, so a program that reads its folder reads a project's files.
func TestARunInNeutralDirStartsThere(t *testing.T) {
	t.Parallel()
	dir := NeutralDir()
	if filepath.Dir(dir) != dir || !filepath.IsAbs(dir) {
		t.Errorf("NeutralDir is %q, want the root of a drive", dir)
	}
	c := helperCmd(t, "pwd")
	c.Dir = dir
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || !sameDir(t, string(res.Stdout), dir) {
		t.Errorf("a run with Dir %q: %v, Code %d, it ran in %q, want there", dir, err, res.Code, res.Stdout)
	}
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	c.Dir = ""
	res, err = Run(t.Context(), c)
	if err != nil || res.Code != 0 || !sameDir(t, string(res.Stdout), here) {
		t.Errorf("a run with an empty Dir: %v, Code %d, it ran in %q, want this process's folder, %q", err, res.Code, res.Stdout, here)
	}
}

// sameDir is whether the folders a and b are one: the same file once each is found.
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	if errA != nil || errB != nil {
		t.Logf("%q: %v; %q: %v", a, errA, b, errB)
		return false
	}
	return os.SameFile(ia, ib)
}
