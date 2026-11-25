package gittest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// porcelain is each command whose output a product parses, run in the repository of
// TestGitsPorcelainOfOneSmallRepositoryIsTheBytesItsSystemHolds: the first three end each record
// with NUL, and log prints one commit in a format that names no locale, time zone or terminal.
func porcelain() [][]string {
	return [][]string{
		{"status", "--porcelain=v2", "-z"},
		{"ls-files", "-s", "-z"},
		{"diff", "--raw", "--no-abbrev", "-z"},
		{"log", "--format=%H %T%n%an <%ae> %ad%n%cn <%ce> %cd%n%s", "--date=raw"},
	}
}

// Under Git's environment, which is gitenv.Env's, git's porcelain output for one small repository is
// the bytes held in testdata: one file for every system, and a file of a system's own beside it
// only where that system's bytes differ. The repository is a commit of a few files, one with a
// non-ASCII name (written in NFC), one executable, one with CRLF line ends and one symbolic link,
// with the identity and both dates fixed, so its object names are the same on every system that
// stores the same tree, and then a work tree with one file changed and one not added. Each
// command's records are held one to a line, which a record holding a newline would break, so none
// may.
//
// The settings git init probes from the disk and writes into the repository (gitenv's doc lists
// them) do not change these bytes on Linux or macOS, which share porcelain.txt. Windows holds
// porcelain_windows.txt, from one run there, and differs at three lines,
// each for a named cause:
//
//   - ls-files' link line is mode 100644, not 120000: core.symlinks is false, so git stores the link
//     as a file whose bytes are its target's name (linksAreFiles: this test writes it so), and the
//     blob is the same.
//   - ls-files' run.sh line is mode 100644, not 100755: core.fileMode is false, so git stores no
//     executable bit, and the blob is the same.
//   - log's first line, the commit and its tree, has other names: the two modes above are in the
//     tree, so its name and the commit's change with them, and nothing else does.
//
// A difference with no named cause is a red, not a new file: a change in what git prints, or a
// variable or setting gitenv.Env lets through.
func TestGitsPorcelainOfOneSmallRepositoryIsTheBytesItsSystemHolds(t *testing.T) {
	t.Parallel()
	dir, git := t.TempDir(), GitPath(t)
	env := append(Git(t.TempDir()), "GIT_AUTHOR_DATE=1700000000 +0000", "GIT_COMMITTER_DATE=1700000000 +0000")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, args...) //nolint:gosec // the host's git, with this test's literal subcommands
		cmd.Dir, cmd.Env = dir, env
		var out, errs bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errs
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errs.String())
		}
		return out.String()
	}
	write := func(name, body string, mode fs.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("README", "one small repository\n", 0o600)
	write("crlf.txt", "one\r\ntwo\r\n", 0o600)
	write("na\u00efve.txt", "a name in NFC\n", 0o600)
	write("run.sh", "#!/bin/sh\necho ran\n", 0o700)
	if linksAreFiles {
		write("link", "README", 0o600)
	} else if err := os.Symlink("README", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "a tree")
	write("README", "one small repository, changed\n", 0o600)
	write("new.txt", "not added\n", 0o600)

	var got strings.Builder
	for _, c := range porcelain() {
		got.WriteString("== git " + strings.Join(c, " ") + "\n")
		out := run(c...)
		if c[len(c)-1] == "-z" {
			for rec := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
				if strings.Contains(rec, "\n") {
					t.Fatalf("git %s printed a record that holds a newline: %q", strings.Join(c, " "), rec)
				}
				got.WriteString(rec + "\n")
			}
			continue
		}
		got.WriteString(out)
	}

	held := filepath.Join("testdata", heldOwn)
	want, err := os.ReadFile(held)
	if errors.Is(err, fs.ErrNotExist) {
		held = filepath.Join("testdata", "porcelain.txt")
		want, err = os.ReadFile(held)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.String() == string(want) {
		return
	}
	g, w := strings.Split(got.String(), "\n"), strings.Split(string(want), "\n")
	for i := range max(len(g), len(w)) {
		if i >= len(g) || i >= len(w) || g[i] != w[i] {
			t.Errorf("line %d of git's output is not %s's:\n%q, want\n%q", i+1, held, at(g, i), at(w, i))
			break
		}
	}
	t.Errorf("git printed, in full:\n%s", got.String())
}

// at is line i of lines, or the words "no line" past its end.
func at(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "no line"
}
