package gitenv_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// who is the identity a commit of a test is made under, set as the caller's own add.
func who() []string {
	return []string{"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com"}
}

// gitIn runs the git of the machine with environ and args, from a folder that is no repository, and
// returns what it printed with its error.
func gitIn(t *testing.T, environ []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // git of the machine, with a test's own arguments
	cmd.Dir = t.TempDir()
	cmd.Env = environ
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// setup runs git as a test sets one up, under Env, and fails the test when it fails.
func setup(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gitIn(t, gitenv.Env(nil, who()...), args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// repo is a repository of its own with one commit and one file, in a folder of the test's own.
func repo(t *testing.T, file string) string {
	t.Helper()
	dir := t.TempDir()
	setup(t, "-C", dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(file+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setup(t, "-C", dir, "add", file)
	setup(t, "-C", dir, "commit", "-q", "-m", "one "+file)
	return dir
}

// program is an executable file that writes a marker file when git runs it, and the marker's path.
// It is written with syscall.ForkLock held for reading, so no fork of this process starts while it
// is open for writing: a child forked then would hold it open until its own exec, and git's start of
// it in that time fails `text file busy` on Linux. The script names the marker with forward slashes,
// a path Git for Windows' sh reads as the system does.
func program(t *testing.T) (path, marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "ran")
	path = filepath.Join(dir, "hook")
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte("#!/bin/sh\n: > '"+filepath.ToSlash(marker)+"'\nexit 0\n"), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return path, marker
}
