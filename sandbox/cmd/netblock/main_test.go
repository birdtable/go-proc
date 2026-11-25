package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/sandbox"
)

// TestTheCommandRefusesALineItDoesNotTakeWithItsUsage: each line Run does not
// take exits 2 and says the usage on stderr, and nothing is run. A line it
// takes would exec the test, so the kernel proof starts the command for
// those.
//
// What it costs when this fails: a line the command does not take runs a program, or exits with no usage,
// and a person's typo is a blocked run of the wrong thing.
func TestTheCommandRefusesALineItDoesNotTakeWithItsUsage(t *testing.T) {
	t.Parallel()
	lines := [][]string{nil, {"--"}, {"/usr/bin/true"}, {"--", "true"}, {"-x", "/usr/bin/true"}}
	for _, args := range lines {
		var stderr strings.Builder
		if code := blocked(args, nil, &stderr); code != codeUsage {
			t.Errorf("%q: exit %d, want %d", args, code, codeUsage)
		}
		if want := "netblock: the blocked command takes --, then the program by its absolute path, then its arguments\n"; stderr.String() != want {
			t.Errorf("%q: said %q, want %q", args, stderr.String(), want)
		}
	}
	if len(lines) == 0 {
		t.Fatal("no lines ran")
	}
}

// TestTheCommandEndsRefusedAndNamesTheBlockItCouldNotSet: a refusal that
// matches ErrNoBlock, on a system that gives none as on a kernel that
// refused a step, exits 5 and says "no block" before the step's own words; a
// line it does not take exits 2 with the usage, never 5; and a program that
// could not be started behind a block that was set exits 1.
//
// What it costs when this fails: a runner reads a missing block as a typo, or as the program's own
// failure, and runs the program again elsewhere with the network open.
func TestTheCommandEndsRefusedAndNamesTheBlockItCouldNotSet(t *testing.T) {
	t.Parallel()
	step := errors.New("landlock speaks ABI 5")
	rows := []struct {
		claim string
		err   error
		code  int
		says  string
	}{
		{"the block not set", noBlockLike(step), codeRefused, "no block: landlock speaks ABI 5"},
		{"ErrNoBlock itself", sandbox.ErrNoBlock, codeRefused, "no block: "},
		{"a line it does not take", sandbox.ErrUsage, codeUsage, sandbox.ErrUsage.Error()},
		{"a program not started behind the block", errors.New("/bin/x could not be started behind the block: no such file"), codeFailed, "/bin/x could not be started"},
	}
	for _, r := range rows {
		code, says := exitFor(r.err)
		if code != r.code || !strings.HasPrefix(says, r.says) {
			t.Errorf("%s: exit %d saying %q, want %d saying %q", r.claim, code, says, r.code, r.says)
		}
	}
	if len(rows) < 4 {
		t.Fatalf("ran %d rows", len(rows))
	}
}

// noBlockLike is err as a refusal that matches sandbox.ErrNoBlock, as the
// block's own refusals do.
func noBlockLike(err error) error { return errors.Join(err, sandbox.ErrNoBlock) }
