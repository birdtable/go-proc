//go:build linux && !live

package proc

import "testing"

// cappedHalf is TestAReadPeakRunUnderACappedParentDoesNotEndLimit's capped half outside a live run,
// which only says, in the test's log, that the half is live: its helper must lower its
// oom_score_adj to -1000, which only a live run's runner lets it. It does not skip, as
// a native run fails on any skip (tests.admission.5). The half itself is in peak_live_linux_test.go.
func cappedHalf(t *testing.T, _ string) {
	t.Helper()
	t.Log("a cgroup is delegated here, and the capped half is live: run it built with -tags live, from its runner")
}
