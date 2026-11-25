//go:build linux && live

package proc

import "testing"

// TestATreeThatGotENOMEMAtTreeMemoryAndExited0EndsCode0: a helper held to 64 MiB, which no oom may
// kill (its oom_score_adj -1000), fallocates 128 MiB of a memfd. Its cgroup counts an oom at its own
// memory.max that kills nothing; the fallocate gets ENOMEM; the helper closes the memfd and exits 0. The
// run ends Code 0 and not `limit`, and its cgroup is gone. Where no cgroup is delegated, the run is
// refused and starts nothing. The row is live: Linux lets a process lower its oom_score_adj only to
// the oom_score_adj_min a privileged process set for it, so it runs only where the test's own parent
// set -1000 there (as root writes -1000 to its own oom_score_adj and starts the test by
// `choom -n 0 --` as another user), and is red anywhere else; a run without the tag has no row to
// skip.
//
// What it costs when this fails: a tree that met its limit, handled the ENOMEM and exited 0 ends
// `limit`, which says the run's limit killed it whole.
func TestATreeThatGotENOMEMAtTreeMemoryAndExited0EndsCode0(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 64 MiB that no oom kills, fallocating 128 MiB of a memfd"
	_, why := delegated(t)
	c := helperCmd(t, "enomem", "128")
	c.Limits = Limits{TreeMemory: treeLimit}
	c.Timeout = exitsWithin
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	got := cgroupLines(res.Stdout)
	if u, ok := got["unkillable"]; ok {
		t.Fatalf("%s: the helper may not lower its oom_score_adj, which a live run needs: %s", what, u)
	}
	if err != nil || res.Code != 0 || got["enomem"] != "true" {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want its fallocate ENOMEM, exit 0 and no error", what, err, res.Code, res.Stdout, res.Stderr)
	}
	cgroupGone(t, what, res.Stdout)
}
