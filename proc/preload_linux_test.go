//go:build linux && cgo

package proc

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shim's argv, as init reads it: argv[0] is shimMark, argv[1] the report's descriptor,
// argv[2] to argv[6] the five strings of its at (the program at[3], its Dir at[4]), and
// the program's own argv follows from argv[7]. shimProgram and shimProgramArgs are those two indexes, as the
// plant execs by them.
const (
	shimProgram     = 5
	shimProgramArgs = 7
)

// skipShim is a library for LD_PRELOAD whose constructor writes "preloaded" and its process's argv[0]
// to stderr, then, in a shim, execs the shim's program at once with the shim's environment, so the
// program would run with the shim's capabilities and before its steps. It execs the program by
// shimProgram and shimProgramArgs, not the cgroups file argv[4] names.
var skipShim = fmt.Sprintf(`#include <stdio.h>
#include <string.h>
#include <fcntl.h>
#include <unistd.h>

extern char **environ;
static char cmdline[1 << 16];

__attribute__((constructor)) static void skip(void) {
	int fd = open("/proc/self/cmdline", O_RDONLY);
	if (fd < 0) return;
	ssize_t n = read(fd, cmdline, sizeof cmdline - 1);
	close(fd);
	if (n <= 0) return;
	char *argv[256];
	int argc = 0;
	for (char *p = cmdline; p < cmdline + n && argc < 255; p += strlen(p) + 1) argv[argc++] = p;
	argv[argc] = NULL;
	dprintf(2, "preloaded %%s\n", argv[0]);
	if (strcmp(argv[0], "%s") == 0 && argc > %d) execve(argv[%d], &argv[%d], environ);
}
`, shimMark, shimProgramArgs, shimProgram, shimProgramArgs)

// TestAContainedRunGivesItsShimNoLoaderVariable: the contained helper run Contained, held to 64 MiB,
// with an Env whose LD_PRELOAD names skipShim, built by cc, reports for itself and a copy of itself
// it runs a CapEff of 0 and no_new_privs set; the library wrote that it was loaded in the helper,
// whose argv[0] is its path, and never in the shim, neither stage of which, shimMark or shimRun as its argv[0], starts with an environment, as a loader
// reads one before the shim's first step. The test
// binary is linked by a loader, as a cgo build is, and cc is a C compiler, or the test fails. Where
// no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: a caller's binary linked by a loader runs code its run's Env names
// in the shim, before the shim's steps: the program runs with CAP_SYS_RESOURCE and CAP_SYS_ADMIN in
// the tree's user namespace, without no_new_privs, user.max_cgroup_namespaces unwritten and the
// caller's /proc, and may nest a cgroup namespace and lift its limit.
func TestAContainedRunGivesItsShimNoLoaderVariable(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded(t, exe) {
		t.Fatalf("%s has no interpreter, so no loader reads LD_PRELOAD in it: build the tests with cgo and without netgo or osusergo", exe)
	}
	dir := t.TempDir()
	src, lib := filepath.Join(dir, "skip.c"), filepath.Join(dir, "skip.so")
	if err := os.WriteFile(src, []byte(skipShim), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "cc", "-shared", "-fPIC", "-o", lib, src).CombinedOutput(); err != nil { //nolint:gosec // G204: cc, on files of the test's
		t.Fatalf("cc: %v: %s", err, out)
	}
	const what = "a Contained helper whose Env preloads a library that skips the shim"
	c := containedCmd(t, what, "caps", "16", "exit", exe)
	c.Env = append(c.Env, "LD_PRELOAD="+lib)
	res, held, err := runContained(t, what, c)
	if !held {
		return
	}
	got := cgroupLines(res.Stdout)
	none, stderr := "0000000000000000", string(res.Stderr)
	inHelper, inShim := strings.Contains(stderr, "preloaded "+exe+"\n"), strings.Contains(stderr, "preloaded "+shimMark)
	if err != nil || !inHelper || inShim || got["owncapeff"] != none || got["ownnnp"] != "1" || got["capeff"] != none || got["nnp"] != "1" {
		t.Errorf("%s: %v, loaded in the helper %t and in the shim %t, its own CapEff %q and NoNewPrivs %q, the copy's %q and %q, stderr %q, want it loaded in the helper alone, CapEff %s and NoNewPrivs 1 for each", what, err, inHelper, inShim, got["owncapeff"], got["ownnnp"], got["capeff"], got["nnp"], stderr, none)
	}
}

// loaded is whether the binary at path names an interpreter, the loader that reads LD_PRELOAD.
func loaded(t *testing.T, path string) bool {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}
