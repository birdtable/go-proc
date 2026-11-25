package gitenv_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/birdtable/go-proc/gitenv"
)

// info is a FileInfo of a test's own making, for the rows that need no file.
type info struct {
	name string
	mode fs.FileMode
}

func (i info) Name() string       { return i.name }
func (i info) Size() int64        { return 0 }
func (i info) Mode() fs.FileMode  { return i.mode }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.mode.IsDir() }
func (i info) Sys() any           { return nil }

// CommitMode refuses a FileInfo that is nil or no regular file, and a head that is none of "",
// 100644 and 100755, each with its own error, which names the file or the head; a refusal gives no
// mode.
//
// What it costs when this fails: a caller commits 100644 or 100755 over a link, a submodule or a
// folder at the path, or for a device it read, and the tree it writes says what the folder does not.
func TestCommitModeRefusesNoRegularFileAndAHeadOfNoFileMode(t *testing.T) {
	t.Parallel()
	regular := info{"f", 0o755}
	refused := 0
	for name, tc := range map[string]struct {
		fi   fs.FileInfo
		head string
		want error
		says string
	}{
		"no FileInfo":           {nil, "", gitenv.ErrNotRegular, ""},
		"a folder":              {info{"dir", fs.ModeDir | 0o755}, "", gitenv.ErrNotRegular, `"dir"`},
		"a link":                {info{"link", fs.ModeSymlink | 0o777}, "", gitenv.ErrNotRegular, `"link"`},
		"a device":              {info{"dev", fs.ModeDevice | 0o600}, "", gitenv.ErrNotRegular, `"dev"`},
		"a named pipe":          {info{"fifo", fs.ModeNamedPipe | 0o600}, "100644", gitenv.ErrNotRegular, `"fifo"`},
		"a socket":              {info{"sock", fs.ModeSocket | 0o600}, "100755", gitenv.ErrNotRegular, `"sock"`},
		"a head that is a link": {regular, "120000", gitenv.ErrHeadMode, `"120000"`},
		"a head that is a tree": {regular, "040000", gitenv.ErrHeadMode, `"040000"`},
		"a submodule's head":    {regular, "160000", gitenv.ErrHeadMode, `"160000"`},
		"a group-writable head": {regular, "100664", gitenv.ErrHeadMode, `"100664"`},
		"a short head":          {regular, "644", gitenv.ErrHeadMode, `"644"`},
		"a head with a space":   {regular, " 100644", gitenv.ErrHeadMode, strconv.Quote(" 100644")},
		"a head with a newline": {regular, "100755\n", gitenv.ErrHeadMode, strconv.Quote("100755\n")},
	} {
		for _, fileMode := range []bool{true, false} {
			refused++
			got, err := gitenv.CommitMode(tc.fi, fileMode, tc.head)
			if !errors.Is(err, tc.want) || got != "" {
				t.Errorf("%s, core.fileMode %v: CommitMode = %q, %v, want the refusal %v", name, fileMode, got, err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("%s, core.fileMode %v: the refusal %q does not name %s", name, fileMode, err, tc.says)
			}
		}
	}
	if refused != 26 {
		t.Fatalf("checked %d of 26 refusals", refused)
	}
}

// CommitMode's table, on every system with FileInfos of the test's own: with core.fileMode true the
// owner's exec bit decides, whatever head and the group's and others' bits say; with it false head
// does, and a new path is 100644.
//
// What it costs when this fails: a file is committed with a mode git would not give it, and the
// next commit made by git itself flips it back.
func TestCommitModeIsTheOwnersBitUnderFileModeAndHeadsModeWithout(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, tc := range []struct {
		perm     fs.FileMode
		fileMode bool
		head     string
		want     string
	}{
		{0o644, true, "", "100644"},
		{0o744, true, "", "100755"},
		{0o655, true, "", "100644"},
		{0o600, true, "100755", "100644"},
		{0o700, true, "100644", "100755"},
		{0o755, true, "100755", "100755"},
		{0o644, false, "", "100644"},
		{0o755, false, "", "100644"},
		{0o644, false, "100755", "100755"},
		{0o755, false, "100644", "100644"},
		{0o755, false, "100755", "100755"},
		{0o644, false, "100644", "100644"},
	} {
		checked++
		got, err := gitenv.CommitMode(info{"f", tc.perm}, tc.fileMode, tc.head)
		if err != nil || got != tc.want {
			t.Errorf("CommitMode(%v, %v, %q) = %q, %v, want %s", tc.perm, tc.fileMode, tc.head, got, err, tc.want)
		}
	}
	if checked != 12 {
		t.Fatalf("checked %d of 12 cases", checked)
	}
}

// modeCase is one case of the git row: HEAD's mode for the path ("" for a new path) and whether the
// file the program writes has the owner's exec bit.
type modeCase struct {
	name string
	head string
	bit  bool
}

// CommitMode gives the mode git itself gives: in a scratch repository with core.fileMode true, false
// or absent, each set by git config, a file is written, a new one with and without the owner's exec
// bit, an existing 100755 path with the bit cleared and an existing 100644 path with it set; git add
// stages it and git ls-files -s says the mode, and CommitMode, given the file's Lstat, the setting
// (absent as true) and HEAD's mode, must say the same. A system that sets no exec bit (Windows) runs
// the 6 cases without one, and the log names the 6 it left out.
//
// What it costs when this fails: a program that writes its own tree commits a mode the repository's
// git would not, and the mode flips at the next commit git makes there.
func TestCommitModeIsTheModeGitAddsInARepository(t *testing.T) {
	t.Parallel()
	cases := []modeCase{
		{"a new file without the bit", "", false},
		{"a new file with the bit", "", true},
		{"a 100755 path with the bit cleared", "100755", false},
		{"a 100644 path with the bit set", "100644", true},
	}
	var ran, left []string
	for _, setting := range []string{"true", "false", "absent"} {
		for _, tc := range cases {
			name := tc.name + " under core.fileMode " + setting
			if tc.bit && !setsExecBit {
				left = append(left, name)
				continue
			}
			ran = append(ran, name)
			dir := t.TempDir()
			setup(t, "-C", dir, "init", "-q", "-b", "main")
			if setting == "absent" {
				setup(t, "-C", dir, "config", "--unset-all", "core.fileMode")
			} else {
				setup(t, "-C", dir, "config", "core.fileMode", setting)
			}
			path := filepath.Join(dir, "f")
			if tc.head != "" {
				write(t, path, "head\n", 0o600)
				chmod := map[string]string{"100644": "--chmod=-x", "100755": "--chmod=+x"}[tc.head]
				setup(t, "-C", dir, "add", chmod, "f")
				setup(t, "-C", dir, "commit", "-q", "-m", "head")
				if got := strings.Fields(setup(t, "-C", dir, "ls-files", "-s", "f")); len(got) == 0 || got[0] != tc.head {
					t.Fatalf("%s: HEAD's mode is %q, so the case is not what it says", name, got)
				}
			}
			perm := fs.FileMode(0o600)
			if tc.bit {
				perm = 0o700
			}
			write(t, path, "written\n", perm)
			fi, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if (fi.Mode().Perm()&0o100 != 0) != tc.bit {
				t.Fatalf("%s: the file's mode is %v, so the case is not what it says", name, fi.Mode())
			}
			setup(t, "-C", dir, "add", "f")
			gits := strings.Fields(setup(t, "-C", dir, "ls-files", "-s", "f"))
			got, err := gitenv.CommitMode(fi, setting != "false", tc.head)
			if len(gits) == 0 || err != nil || got != gits[0] {
				t.Errorf("%s: CommitMode = %q, %v; git add staged %q", name, got, err, gits)
			}
		}
	}
	t.Logf("ran %d cases: %q", len(ran), ran)
	if len(left) > 0 {
		t.Logf("this system sets no exec bit, so these did not run: %q", left)
	}
	// Two of the four cases have the bit set, so a system that sets none leaves out those two under
	// each of the three settings: 6 run there, and all 12 elsewhere.
	want := 12
	if !setsExecBit {
		want = 6
	}
	if len(ran)+len(left) != 12 || len(ran) != want {
		t.Fatalf("ran %d and left %d of 12 cases, want %d run on this system", len(ran), len(left), want)
	}
}

// write writes body to path, then sets its permission bits to perm, as WriteFile keeps an existing
// file's.
func write(t *testing.T, path, body string, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// A fresh git init sets core.fileMode as this system's git does: false on Windows, where Git for
// Windows makes every repository so, and true elsewhere, where git finds the exec bit kept. Read the
// way CommitMode's doc gives, git config --type=bool --get core.fileMode. Unset, git config prints
// nothing and exits 1, the absent case the doc reads as true.
//
// What it costs when this fails: CommitMode's doc says what git does on a system, and a caller that
// trusts it reads the setting wrongly or not at all.
func TestAFreshRepositoryHasTheFileModeThisSystemsGitGivesIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	setup(t, "-C", dir, "init", "-q", "-b", "main")
	if got := setup(t, gitenv.ReadArgs(dir, "config", "--type=bool", "--get", "core.fileMode")...); got != initFileMode {
		t.Errorf("a fresh repository's core.fileMode is %q, want %s on this system", got, initFileMode)
	}
	setup(t, "-C", dir, "config", "--unset-all", "core.fileMode")
	out, err := gitIn(t, gitenv.Env(nil, who()...), gitenv.ReadArgs(dir, "config", "--type=bool", "--get", "core.fileMode")...)
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || out != "" {
		t.Errorf("unset, git config --get core.fileMode printed %q (%v), want nothing and exit 1", out, err)
	}
}
