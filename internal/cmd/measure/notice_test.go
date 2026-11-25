package main

import (
	"bytes"
	"strings"
	"testing"
)

// noticeMod is a go.mod of the shape go-proc's has: a module line, one required module on a line
// of its own and two in a block, one of them indirect.
const noticeMod = `module example.com/lib

go 1.27.1

require golang.org/x/sys v0.49.0

require (
	github.com/shirou/gopsutil/v4 v4.26.10
	github.com/ebitengine/purego v0.9.1 // indirect
)
`

// noticeText names the two modules of noticeBuild, the second over a line's end as a NOTICE
// wraps, and the indirect one by its short name, as go-proc's does.
const noticeText = `lib

The library uses these modules, each under its own licence:

- golang.org/x/sys v0.49.0, which the library imports itself: BSD-3-Clause.
- github.com/shirou/gopsutil/v4
  v4.26.10, by which it reads the load: BSD-3-Clause. ebitengine/purego is in no build.
  See https://example.com/notes (http://www.example.com/).
`

// noticeBuild is what the notice-check task pipes in: go list's lines for three systems, the
// module's own with no version, and repeats.
const noticeBuild = "example.com/lib \ngolang.org/x/sys v0.49.0\ngithub.com/shirou/gopsutil/v4 v4.26.10\n" +
	"example.com/lib \ngolang.org/x/sys v0.49.0\n\nexample.com/lib \ngolang.org/x/sys v0.49.0\n"

// noticeCheck runs notice-check in a tree of go.mod and NOTICE, with build on its input, and
// gives the exit code and what it said.
func noticeCheck(t *testing.T, notice, build string) (int, string) {
	t.Helper()
	dir := tree(t, map[string]string{"go.mod": noticeMod, "NOTICE": notice})
	var out bytes.Buffer
	code := run([]string{"notice-check"}, dir, strings.NewReader(build), &out)
	return code, out.String()
}

// What it costs when this fails: the release tier refuses a NOTICE that is right, wrapped or with a
// URL in it, and the tag waits on a check that is wrong.
func TestNoticeCheckPassesANoticeThatNamesEachModuleOfTheBuildAtItsVersion(t *testing.T) {
	t.Parallel()
	if code, out := noticeCheck(t, noticeText, noticeBuild); code != 0 || out != "" {
		t.Errorf("notice-check exited %d and said %q, and want 0 and nothing", code, out)
	}
}

// What it costs when this fails: a tag ships a NOTICE that names a version of a module the
// library does not ship, or names none for one it does.
func TestNoticeCheckRefusesAModuleOfTheBuildNotNamedAtItsVersion(t *testing.T) {
	t.Parallel()
	for name, notice := range map[string]string{
		"its version changed": strings.Replace(noticeText, "v4.26.10", "v4.26.9", 1),
		"left out":            strings.Replace(noticeText, "golang.org/x/sys v0.49.0", "x/sys", 1),
		"a longer version":    strings.Replace(noticeText, "v0.49.0,", "v0.49.01,", 1),
	} {
		code, out := noticeCheck(t, notice, noticeBuild)
		module := "github.com/shirou/gopsutil/v4"
		if name != "its version changed" {
			module = "golang.org/x/sys"
		}
		if want := module + " is in the build and NOTICE does not name it"; code != 1 || !strings.Contains(out, want) {
			t.Errorf("%s: notice-check exited %d and said %q, and want 1 and %q", name, code, out, want)
		}
	}
}

// What it costs when this fails: a NOTICE keeps an entry for a module go.mod dropped, as v0.8.0,
// v0.8.1 and v0.9.0 shipped the cgroups entry.
func TestNoticeCheckRefusesAModuleGoModDoesNotList(t *testing.T) {
	t.Parallel()
	notice := noticeText + "- github.com/containerd/cgroups/v3 v3.1.3, with github.com/coreos/go-systemd/v22\n  v22.5.0 (Apache-2.0).\n"
	code, out := noticeCheck(t, notice, noticeBuild)
	for _, want := range []string{
		"NOTICE names github.com/containerd/cgroups/v3, which go.mod does not list",
		"NOTICE names github.com/coreos/go-systemd/v22, which go.mod does not list",
	} {
		if code != 1 || !strings.Contains(out, want) {
			t.Errorf("notice-check exited %d and said %q, and want 1 and %q", code, out, want)
		}
	}
	if strings.Contains(out, "example.com/notes") || strings.Contains(out, "purego") {
		t.Errorf("notice-check named a URL or a short name as a module: %q", out)
	}
}

// What it costs when this fails: the task's go list fails or prints nothing, and the check passes
// having read no module.
func TestNoticeCheckRefusesToCheckWithNoModuleRead(t *testing.T) {
	t.Parallel()
	for name, build := range map[string]string{
		"nothing":          "",
		"the module alone": "example.com/lib \n",
		"not go list's":    "golang.org/x/sys\n",
	} {
		if code, out := noticeCheck(t, noticeText, build); code != 2 {
			t.Errorf("%s: notice-check exited %d and said %q, and want 2", name, code, out)
		}
	}
	dir := tree(t, map[string]string{"go.mod": noticeMod, "NOTICE": noticeText})
	var out bytes.Buffer
	if code := run([]string{"notice-check"}, dir, nil, &out); code != 2 {
		t.Errorf("notice-check with no input exited %d, and want 2: %s", code, &out)
	}
}
