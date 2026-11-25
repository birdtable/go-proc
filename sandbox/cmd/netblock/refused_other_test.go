//go:build !linux && !darwin

package main

import (
	"os"
	"strings"
	"testing"
)

// TestTheCommandIsRefusedWhereTheSystemGivesNoBlock: on Windows and the BSDs,
// a line the command takes, a program by its absolute path, ends refused,
// exit 5, saying the block is missing, never the usage's 2, and the program
// is not run: the test binary named is still this one, running.
//
// What it costs when this fails: on a system with no block, a runner is told its line is wrong, or the
// program runs with the network open.
func TestTheCommandIsRefusedWhereTheSystemGivesNoBlock(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	code := blocked([]string{"--", self, "-test.run=^$"}, nil, &stderr)
	if code != codeRefused || !strings.HasPrefix(stderr.String(), "netblock: no block: ") {
		t.Errorf("exit %d saying %q, want %d and no block named", code, stderr.String(), codeRefused)
	}
}
