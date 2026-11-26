package proc

import (
	"errors"
	"slices"
	"testing"
)

// TestTheShimsFirstStageGivesTheSecondGODEBUGContainermaxprocsZeroAlone: the first stage's exec of
// the second, through an exec of the test's own in place of syscall.Exec, starts this binary by its
// proc's self/exe, with shimRun, the report's descriptor, the program's path and the program's argv,
// and with exactly GODEBUG=containermaxprocs=0 as its environment, none of the caller's. The exec's
// error is what it returns. The row needs no cgroup, no namespace and no capability, so the push tier
// holds it on any Linux.
//
// What it costs when this fails: the second stage's Go runtime keeps the cpu.max it opens under the
// shim's cgroup cover, the caller's check of the run's threads reads that descriptor as a forgery,
// and every healthy run on a host that delegates cpu is killed; the +cpu row, which alone held this
// before, runs only where cpu is delegated, so the push tier stayed green.
func TestTheShimsFirstStageGivesTheSecondGODEBUGContainermaxprocsZeroAlone(t *testing.T) {
	t.Parallel()
	refused := errors.New("the test's exec, which runs nothing")
	var path string
	var argv, env []string
	err := execSecondStage(func(argv0 string, a, e []string) error {
		path, argv, env = argv0, slices.Clone(a), slices.Clone(e)
		return refused
	}, 7, "/usr/bin/prog", []string{"prog", "-x"})
	if !errors.Is(err, refused) {
		t.Errorf("execSecondStage: %v, want the exec's own error", err)
	}
	if path != "/proc/self/exe" {
		t.Errorf("the second stage is exec'd at %q, want /proc/self/exe", path)
	}
	if want := []string{shimRun, "7", "/usr/bin/prog", "prog", "-x"}; !slices.Equal(argv, want) {
		t.Errorf("the second stage's argv: %q, want %q", argv, want)
	}
	if want := []string{"GODEBUG=containermaxprocs=0"}; !slices.Equal(env, want) {
		t.Errorf("the second stage's environment: %q, want %q alone", env, want)
	}
}
