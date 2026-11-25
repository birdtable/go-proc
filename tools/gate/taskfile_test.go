package main

import (
	"testing"

	"github.com/birdtable/go-testkit/taskfile"
)

// TestTheTaskfileCallsGo holds Taskfile.yml to Go: no shell utility, pipe,
// redirection, shell loop, /tmp path or path joined in shell in any task's commands, but for the
// commands taskfile-exceptions.txt keeps in shell with their reasons, and no line there the check
// does not need.
func TestTheTaskfileCallsGo(t *testing.T) {
	t.Parallel()
	taskfile.Check(t, "../..")
}
