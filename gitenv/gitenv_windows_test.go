//go:build windows

package gitenv_test

import (
	"os"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// Env keeps SystemRoot, whatever its case, and a git started with the result runs: without SystemRoot
// a Windows program cannot start its sockets or its crypto, and git exits before it answers.
//
// What it costs when this fails: Env drops a variable Windows needs, and every child git started
// under it dies before it reads a folder, on the one system the other tests cannot see it on.
func TestEnvKeepsSystemRootAndGitStartsWithTheResult(t *testing.T) {
	t.Parallel()
	if _, ok := os.LookupEnv("SystemRoot"); !ok {
		t.Fatal("the test process has no SystemRoot, so the case proves nothing")
	}
	env := gitenv.Env(os.Environ(), who()...)
	kept := false
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); strings.EqualFold(name, "SystemRoot") {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("Env dropped SystemRoot: %q", env)
	}
	out, err := gitIn(t, env, "--version")
	if err != nil || !strings.HasPrefix(out, "git version") {
		t.Errorf("git did not start under Env: %v\n%s", err, out)
	}
}
