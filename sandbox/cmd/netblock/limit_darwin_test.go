//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheProgramStartsWithTheOpenFileLimitTheCallerStartedWith: on macOS, a caller started with a
// soft open-file limit of 200, and then of 4096, runs the command, and the program behind the
// block, /bin/sh, says its soft limit is that one. The command is a Go program, whose runtime
// raises the soft limit as it starts: the control is the row child, a Go program started the same
// way with no block, which says a limit that is not the one it started with. syscall.Exec puts
// the limit back before its exec and posix_spawn does not, so the block first calls syscall.Exec
// for no path.
//
// What it costs when this fails: a program behind the block starts with the runtime's raised
// limit, hundreds of thousands of descriptors where its caller was given a few hundred, and one
// that sizes a table by the limit, or closes every descriptor up to it, is slow or fails.
func TestTheProgramStartsWithTheOpenFileLimitTheCallerStartedWith(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	limits := []string{"200", "4096"}
	for _, soft := range limits {
		report := filepath.Join(t.TempDir(), "raised")
		code, out := p.start(t, nil, nil, limitedWord, soft, rowWord, report, "limit")
		raised, err := os.ReadFile(report)
		if code != 0 || err != nil {
			t.Fatalf("started with a soft limit of %s, the control did not run: exit %d, %v: %s", soft, code, err, out)
		}
		if string(raised) == soft {
			t.Fatalf("a Go program started with a soft limit of %s still has it, so its runtime raised nothing and the row proves nothing", soft)
		}
		code, out = p.start(t, nil, nil, limitedWord, soft, asCommand, "--", "/bin/sh", "-c", "ulimit -S -n")
		if said := strings.TrimSpace(out); code != 0 || said != soft {
			t.Errorf("a caller started with a soft open-file limit of %s: the program behind the block says %q (exit %d), want %s; the Go program started so says %s", soft, said, code, soft, raised)
		}
	}
	if len(limits) < 2 {
		t.Fatalf("ran %d limits", len(limits))
	}
}
