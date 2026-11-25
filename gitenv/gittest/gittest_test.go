package gittest

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// TestGitGivesAChildItsOwnConfigurationAndAnIdentityAndNoOtherGitName: Git of a folder is Clean of
// the process's environment with git's global configuration in a file of the folder, the system's
// off, no prompt and an identity, and with no GIT_ name of Isolate's or the run's beside them, and
// HOME as the folder: gitenv takes the variables that name the person's home, and Git
// names one of the test's. It ends with http.emptyAuth=false and an empty ticket cache
// in the folder, wanted by their values, not by noTicket's, so a noTicket that sets
// another value or drops KRB5CCNAME is red here on a host that holds no ticket.
//
// What it costs when this fails: a test's git reads or writes the person's configuration, waits on a
// prompt, or cannot commit; or, with no HOME, libcurl reads the .netrc of the account's home and git
// sends the person's password to a host it names; or git spends the person's Kerberos ticket on a
// Negotiate host.
func TestGitGivesAChildItsOwnConfigurationAndAnIdentityAndNoOtherGitName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := Git(dir)
	gits := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			gits++
		}
	}
	ssh := byName(env)["GIT_SSH_COMMAND"]
	want := slices.Concat([]string{"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "HOME=" + dir, gitConfig + "=" + filepath.Join(dir, "gitconfig")}, who(), []string{"GIT_SSH_COMMAND=" + ssh},
		[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.emptyAuth", "GIT_CONFIG_VALUE_0=false", "KRB5CCNAME=FILE:" + filepath.Join(dir, "krb5cc")})
	if !slices.Equal(shellWords(ssh), sshArgs(dir)) {
		t.Errorf("Git(%s) gives GIT_SSH_COMMAND %q, which the shell reads as %q, want %q", dir, ssh, shellWords(ssh), sshArgs(dir))
	}
	if gits != len(want)-2 || !slices.Equal(env[len(env)-len(want):], want) { // HOME and KRB5CCNAME are the two not GIT_
		t.Errorf("Git(%s) ends\n%q\nwith %d GIT_ names, want it to end\n%q\nand hold no other", dir, env, gits, want)
	}
	got := byName(env)
	if got["PATH"] != os.Getenv("PATH") || got["HOME"] != dir {
		t.Errorf("Git(%s) gives PATH %q and HOME %q, want the process's PATH and HOME %s", dir, got["PATH"], got["HOME"], dir)
	}
}

// Every variable gitenv.Env strips, gitFrom strips too, whatever Clean let through: a GIT_ name in any
// case, a numbered configuration name and the variables that name a home. Git's stripping is
// gitenv's, not a copy of its own that can fall behind.
//
// What it costs when this fails: gitenv learns a name that moves git, and a test's git child,
// which Git makes, still carries it.
func TestGitStripsWhatGitenvStrips(t *testing.T) {
	t.Parallel()
	planted := []string{"git_dir=/elsewhere", "Git_Index_File=/elsewhere/index", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/elsewhere",
		"GIT_CONFIG_KEY_1=core.fsmonitor", "GIT_CONFIG_VALUE_1=/elsewhere/fsmonitor", // a count of 2, not the 1 Git sets itself
		"HOME=/home/a", "USERPROFILE=C:\\a", "HOMEDRIVE=C:", "HOMEPATH=\\a", "XDG_CONFIG_HOME=/home/a/.config", "NETRC=/home/a/.netrc", "PATH=/bin"}
	kept := gitenv.Env(planted)
	out := gitFrom(planted, t.TempDir())
	checked := 0
	for _, kv := range planted {
		if slices.Contains(kept, kv) {
			continue
		}
		checked++
		if slices.Contains(out, kv) {
			t.Errorf("gitenv.Env strips %s, and Git keeps it", kv)
		}
	}
	if checked != len(planted)-1 || !slices.Contains(out, "PATH=/bin") {
		t.Fatalf("gitenv stripped %d of the %d planted variables, want all but PATH, and Git gave %q", checked, len(planted), out)
	}
}

// Repo's tree holds each file as a plain file with its bytes and each link at git's link mode with its
// target, and no link is made on disk.
//
// What it costs when this fails: a test of a repository with a link is a test of a plain file, or
// cannot run where this account may make no link.
func TestARepoHoldsItsFilesAndItsLinksCommitted(t *testing.T) {
	t.Parallel()
	dir := Repo(t, map[string]string{"AGENTS.md": "# Agents\n", "docs/a.md": "a\n"}, map[string]string{"CLAUDE.md": "AGENTS.md"})
	show := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), GitPath(t), args...) //nolint:gosec // the host's git, with the test's literal arguments
		cmd.Dir, cmd.Env = dir, Git(t.TempDir())
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(out)
	}
	modes := map[string]string{}
	for line := range strings.Lines(show("ls-tree", "-r", "HEAD")) {
		if meta, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t"); ok {
			modes[name] = strings.Fields(meta)[0]
		}
	}
	for name, want := range map[string]string{"AGENTS.md": "100644", "docs/a.md": "100644", "CLAUDE.md": "120000"} {
		if modes[name] != want {
			t.Errorf("HEAD's tree holds %s at mode %q, want %s (the tree: %v)", name, modes[name], want, modes)
		}
	}
	if got := show("cat-file", "-p", "HEAD:CLAUDE.md"); got != "AGENTS.md" {
		t.Errorf("the link's target = %q, want AGENTS.md", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "CLAUDE.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the link is on disk: %v", err)
	}
}

// byName is environ by name, the last value a name is given winning, as a process reads it.
func byName(environ []string) map[string]string {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, _ := strings.Cut(kv, "="); k != "" {
			env[k] = v
		}
	}
	return env
}
