//go:build windows

package proc

import (
	"bytes"
	"testing"
)

// TestATreeProcessLiftsItsJobsMemoryLimitAsLimitsSays: a helper held to 256 MiB opens
// its caller, the test binary that ran it, with PROCESS_DUP_HANDLE, duplicates each of the caller's
// handles that is a job it is in, clears JOB_OBJECT_LIMIT_JOB_MEMORY on it, then allocates 1 GiB.
// Limits.TreeMemory's doc says a process of the tree may do so, and the run then ends as if it had
// no limit: Code 0, its output whole. Its stdout says each step: "caller opened" or "caller
// refused", then "job <handle> lifted from <bytes>" or why not for each job found, then "found <n>"
// and "done". It lifts only the jobs it is in, so another test's run keeps its limit.
//
// What it costs when this fails: Windows refuses what the doc says a tree may do, or the job no
// longer holds the limit the helper found, and the stated limit no longer says what Windows does.
// Read the doc and the README again.
func TestATreeProcessLiftsItsJobsMemoryLimitAsLimitsSays(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "lift-hog", "1024")
	c.Limits = Limits{TreeMemory: 256 << 20}
	res, err := Run(t.Context(), c)
	want := "caller opened\njob "
	if err != nil || res.Code != 0 || !bytes.HasPrefix(res.Stdout, []byte(want)) ||
		!bytes.HasSuffix(res.Stdout, []byte(" lifted from 268435456\nfound 1\ndone\n")) {
		t.Errorf("a helper that lifts its job's limit of 256 MiB, then allocates 1 GiB: %v, Code %d, stdout %q, stderr %q, want nil, Code 0, and its job found, lifted from 268435456, and done", err, res.Code, res.Stdout, res.Stderr)
	}
}
