package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests read trees they write in t.TempDir. The package has no TestMain of testkit: it starts
// no process and reads no person's folder, and it imports the standard library alone.

// tree writes files, each a path from the root and its text, into a new folder and gives it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// lines gives n lines of Go comment, each ended by a newline.
func lines(n int) string { return strings.Repeat("// a line\n", n) }

// What it costs when this fails: the size a tag is held to counts tests, test support or test
// data, or misses a system's file, and a tag passes or fails on lines no program links.
func TestSizeCountsTheNonTestLinesAndRefusesAboveTheCeiling(t *testing.T) {
	t.Parallel()
	files := map[string]string{
		"a/a.go":            lines(1000),
		"a/b/b_windows.go":  lines(Ceiling - 1007),
		"a/c_linux.go":      lines(7),
		"a/a_test.go":       lines(5000),
		"a/testdata/t.go":   lines(5000),
		"internal/x/x.go":   lines(5000),
		".hidden/h.go":      lines(5000),
		"a/_old/o.go":       lines(5000),
		"tools/gate/go.mod": "module example.com/gate\n",
		"tools/gate/g.go":   lines(5000),
		"a/notes.txt":       lines(5000),
	}
	dir := tree(t, files)
	var out bytes.Buffer
	if code := run([]string{"size"}, dir, nil, &out); code != 0 {
		t.Errorf("size exited %d at the ceiling, and want 0: %s", code, &out)
	}
	if want := "runtime packages (all but testenv): 11867\ntestenv: 0\nnon-test Go lines outside internal/: 11867 (the ceiling is 11867)\n"; out.String() != want {
		t.Errorf("size said %q, and want %q", &out, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "a.go"), []byte(lines(1001)), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{"size"}, dir, nil, &out); code != 1 {
		t.Errorf("size exited %d at 11,868 lines, and want 1: %s", code, &out)
	}
	if want := "11868 lines is 1 above the ceiling"; !strings.Contains(out.String(), want) {
		t.Errorf("size did not say %q:\n%s", want, &out)
	}
}

// What it costs when this fails: a tag's notes give testenv's lines as the runtime's, or the
// other way round, and the two counts say nothing true of either part.
func TestSizePrintsTheRuntimeAndTestenvCountsBesideTheTotal(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{
		"proc/p.go":                    lines(100),
		"sandbox/cmd/netblock/main.go": lines(20),
		"testenvx/z.go":                lines(11),
		"testenv/x.go":                 lines(30),
		"testenv/sub/y.go":             lines(5),
		"testenv/x_test.go":            lines(500),
	})
	var out bytes.Buffer
	if code := run([]string{"size"}, dir, nil, &out); code != 0 {
		t.Errorf("size exited %d under the ceiling, and want 0: %s", code, &out)
	}
	want := "runtime packages (all but testenv): 131\ntestenv: 35\n" +
		fmt.Sprintf("non-test Go lines outside internal/: 166 (the ceiling is %d)\n", Ceiling)
	if out.String() != want {
		t.Errorf("size said %q, and want %q", &out, want)
	}
}

// What it costs when this fails: a task run with no word or a misspelt one passes and checks
// nothing.
func TestRunTakesOneWordItKnows(t *testing.T) {
	t.Parallel()
	dir := tree(t, map[string]string{"a/a.go": "package a\n"})
	for _, args := range [][]string{nil, {"size", "notice-check"}, {"api"}, {"api-check"}} {
		var out bytes.Buffer
		if code := run(args, dir, nil, &out); code != 2 {
			t.Errorf("run(%q) exited %d, and want 2: %s", args, code, &out)
		}
	}
}
