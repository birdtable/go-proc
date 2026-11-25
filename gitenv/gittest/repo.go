package gittest

import (
	"bytes"
	"os"
	"os/exec" //nolint:depguard // gittest is a leaf and cannot import the proc runner; a test's git runs under t's context
	"path/filepath"
	"strings"
	"testing"
)

// GitPath is the absolute path of the git this host puts on its PATH, found as the host decides, with no
// list of folders a host might keep it in. A test hands it to the code under test as its setting, as a
// person does. A host with no git fails t.
func GitPath(t testing.TB) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test runs the real git, and the host's PATH holds none: %v", err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// Repo is a git repository in a temp folder of t's own, with files (path to bytes, slash-separated)
// committed once on git's own default branch, and is its folder. Repo names no branch, and Git's
// environment reads no init.defaultBranch, so the branch is the one the host's git was built with;
// a test that needs a name asks git (symbolic-ref --short HEAD) or renames it. Each path of links
// is a symbolic link to its target, committed through git's index and never made on disk, so the
// repository holds a link where this account may make none, and its work tree lacks the path. git
// is the host's (GitPath), run with Git's environment of a folder of its own, so none of the host's
// configuration is read, and an identity of Git's.
func Repo(t testing.TB, files, links map[string]string) string {
	t.Helper()
	dir, git, env := t.TempDir(), GitPath(t), Git(t.TempDir())
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, args...) //nolint:gosec // the host's git, with Repo's literal subcommands and the test's own paths
		cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(stdin)
		var out, errs bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errs
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errs.String())
		}
		return strings.TrimSpace(out.String())
	}
	run("", "init", "-q")
	run("", "add", "-A")
	for name, target := range links {
		blob := run(target, "hash-object", "-w", "--stdin")
		run("", "update-index", "--add", "--cacheinfo", "120000,"+blob+","+name)
	}
	run("", "commit", "-q", "-m", "a tree")
	return dir
}
