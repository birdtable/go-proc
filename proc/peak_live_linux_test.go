//go:build linux && live

package proc

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the capped parent is marked delegated by xattr and opened O_PATH, which syscall does not give
)

// cappedParent is a delegated cgroup made for a test beside this process's, in the delegated parent p:
// marked as systemd marks one, its memory.max mib MiB with no swap, its children given memory, and a
// leaf in it for a process to start in, open as the folder that start takes. The test's cleanup kills
// what is left in it and removes it.
func cappedParent(t *testing.T, p string, mib int) *os.File {
	t.Helper()
	q := filepath.Join(p, "capped-"+strings.TrimPrefix(runName(), runPrefix))
	if err := os.Mkdir(q, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		q := &runCgroup{path: q, pause: killPaused}
		if err := q.remove(); err != nil {
			t.Error(err)
		}
	})
	if err := unix.Setxattr(q, delegateMark, []byte(delegateValue), 0); err != nil {
		t.Fatalf("marking %s delegated: %v", q, err)
	}
	for _, w := range [][2]string{{"memory.max", strconv.Itoa(mib << 20)}, {"memory.swap.max", "0"}, {"cgroup.subtree_control", "+memory"}} {
		if err := os.WriteFile(filepath.Join(q, w[0]), []byte(w[1]), 0); err != nil {
			t.Fatalf("writing %s of %s: %v", w[0], q, err)
		}
	}
	leaf := filepath.Join(q, "leaf")
	if err := os.Mkdir(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenFile(leaf, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	return dir
}

// cappedHalf is TestAReadPeakRunUnderACappedParentDoesNotEndLimit's capped half, in the delegated
// parent p: a helper started in a delegated cgroup capped at 64 MiB, which no oom may kill (its
// oom_score_adj -1000), runs a hog of 128 MiB with ReadPeak and no TreeMemory. The cap above the
// run's cgroup kills the hog, which raised its oom_score_adj to the most first. So the run ends
// Code -1, but not `limit`: the run set no limit, and a kill for a limit above its cgroup is not one
// of its. The row's other half, where no cgroup is delegated, is in cgroup_linux_test.go and runs in
// every build.
//
// The helper must be one no oom may kill. The killed hog's memory stays charged to the cap as it
// exits, after the kernel no longer counts the hog as a victim, so a page the helper touches then is
// a second oom of the cap, and a helper an oom may kill is its victim. At -1000 the
// helper's fault waits instead until the hog's memory is freed. The row is live: Linux lets a
// process lower its oom_score_adj only to the oom_score_adj_min a privileged process set for it, as
// TestATreeThatGotENOMEMAtTreeMemoryAndExited0EndsCode0 says.
func cappedHalf(t *testing.T, p string) {
	t.Helper()
	const what = "a ReadPeak run of a hog of 128 MiB under a parent capped at 64 MiB"
	dir := cappedParent(t, p, 64)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	helper := &process{Path: exe, Args: []string{exe, helperMode + "peak-run", "128"}, Env: Bare("GORACE"), Stdout: &out, Stderr: &errOut,
		SysProcAttr: &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd())}}
	if err := startTestProcess(helper); err != nil {
		t.Fatal(err)
	}
	werr := helper.Wait()
	got := cgroupLines(out.Bytes())
	if u, ok := got["unkillable"]; ok {
		t.Fatalf("%s: the helper may not lower its oom_score_adj, which a live run needs: %s", what, u)
	}
	if werr != nil || got["limit"] != "false" || got["code"] != "-1" {
		t.Errorf("%s: helper %v, stdout %q, stderr %q, want the inner run killed by the cap above it, Code -1, and not limit", what, werr, out.Bytes(), errOut.Bytes())
	}
}
