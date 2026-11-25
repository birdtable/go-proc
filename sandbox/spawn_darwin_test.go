//go:build darwin

package sandbox

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// limitExec is the macOS kernel, with its real dry run, whose exec is the block's own spawnExec
// with gave as what syscall.Exec of no path gives, and missing, a path that names nothing, as the
// program: a spawn that is reached fails with ENOENT of its own, and this process stays the test.
type limitExec struct {
	darwinKernel
	gave    error
	missing string
}

func (k limitExec) exec(_ string, argv, env []string) error {
	return spawnExec(func(string, []string, []string) error { return k.gave }, k.missing, argv, env)
}

// TestAnExecOfNoPathThatGivesAnythingButENOENTRefusesTheRunWithNothingSpawned: on macOS the block
// puts the open-file limit back by a syscall.Exec of no path before its spawn, and when that
// gives another error, or none, the run is refused, matching ErrNoBlock and naming what it gave,
// and the spawn is not reached. The control is the exec giving ENOENT: the spawn is reached, and
// says ENOENT of its own for a program that is not there.
//
// What it costs when this fails: a Go whose syscall.Exec no longer fails for no path as it did
// runs the program with a limit nobody checked, where the block's doc says the caller's.
func TestAnExecOfNoPathThatGivesAnythingButENOENTRefusesTheRunWithNothingSpawned(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-program")
	const refusal = "the open-file limit this process started with could not be put back: syscall.Exec of no path gave "
	rows := []struct {
		claim string
		gave  error
		says  string
	}{
		{"another error", syscall.EPERM, refusal + `operation not permitted, where it gives "no such file or directory"`},
		{"no error", nil, refusal + `no error, where it gives "no such file or directory"`},
		{"ENOENT, the control", syscall.ENOENT, ""},
	}
	for _, r := range rows {
		err := runMac(limitExec{gave: r.gave, missing: missing}, named{}, missing, []string{missing}, nil)
		refusedAsNoBlock(t, r.claim, err, nil)
		if r.says == "" {
			if !errors.Is(err, syscall.ENOENT) || strings.Contains(err.Error(), refusal) {
				t.Errorf("%s: %v; want the spawn reached, and its own ENOENT for a program that is not there", r.claim, err)
			}
			continue
		}
		if errors.Is(err, syscall.ENOENT) || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v; want the run refused before the spawn, saying %q", r.claim, err, r.says)
		}
	}
	if len(rows) < 3 {
		t.Fatalf("ran %d rows", len(rows))
	}
}
