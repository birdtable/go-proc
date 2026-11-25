//go:build unix

package proc

import (
	"strconv"
	"strings"
	"testing"
)

// TestChildrenCPUCountsAChildOnceItIsWaitedFor: a helper with no child of its own reads ChildrenCPU,
// starts a burn that takes burnFor of CPU time, at least half in user code and the rest in either, reads it
// again once the burn says it is done but before its wait, and again after. Each read gives no error;
// the read before the wait gives what the first did, and the read after it at least that plus the
// user and system time the burn read of itself before it ended.
//
// What it costs when this fails: a caller that reads its children's CPU, as to bill or bound the
// programs it ran, reads its own time, the user time alone, or a child it has not waited for, and
// its figure is wrong by up to all of a child's time.
func TestChildrenCPUCountsAChildOnceItIsWaitedFor(t *testing.T) {
	t.Parallel()
	res, err := Run(t.Context(), helperCmd(t, "children-cpu"))
	got := map[string]int64{}
	for line := range strings.Lines(string(res.Stdout)) {
		f := strings.Fields(line)
		for i, v := range f[1:] {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				got[f[0]+strconv.Itoa(i)] = n
			}
		}
	}
	before, running, after, user, sys := got["before0"], got["running0"], got["after0"], got["burnt0"], got["burnt1"]
	if err != nil || len(got) != 5 || running != before || after-before < user+sys || user < int64(burnFor/2) || user+sys < int64(burnFor) {
		t.Errorf("a helper that read ChildrenCPU around a burn: %v, stdout %q, stderr %q, want before, running the same as before, and after at least before plus the burn's own user and system time, the burn's user time at least %v and its user and system time together at least %v", err, res.Stdout, res.Stderr, burnFor/2, burnFor)
	}
}
