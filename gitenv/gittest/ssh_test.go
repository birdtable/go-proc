package gittest

import (
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestAGitChildsSSHReadsNothingOfThePersonsHome: the ssh a git child of Git(dir) runs, read back
// through ssh -G as git itself runs it, loads no identity file, offers only a key it names, and keeps
// its known hosts in dir, and reads no configuration file, the person's ~/.ssh/config and the system's
// ssh_config among them, which could add a key, a ProxyCommand or a Match exec: nothing under the .ssh of the account's home, which ssh takes for ~ on every
// system whatever HOME says, from the passwd entry or from Windows' profile. The row
// adds -v -G and sends its output to stderr in the GIT_SSH_COMMAND git runs through its shell, so it is
// the ssh git finds, Git for Windows' own among them; ssh -G connects to nothing and reads no key.
//
// What it costs when this fails: a test's git over ssh reads the person's ~/.ssh/config, checks hosts
// against their known_hosts, and offers their keys, so it may authenticate as them.
func TestAGitChildsSSHReadsNothingOfThePersonsHome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := Git(dir)
	ssh := "ssh"
	if v, ok := byName(env)["GIT_SSH_COMMAND"]; ok {
		ssh = v
	}
	cmd := exec.CommandContext(t.Context(), GitPath(t), "ls-remote", "ssh://probe.invalid/r.git") //nolint:gosec // the host's git, whose ssh prints its configuration and connects to nothing
	cmd.Env, cmd.Dir = append(env, "GIT_SSH_COMMAND="+ssh+" -v -G probe.invalid 1>&2 #"), dir
	out, _ := cmd.CombinedOutput() // ssh -G speaks no protocol, so git fails
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := samePath(filepath.Join(u.HomeDir, ".ssh"))
	var ids []string
	conf := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if strings.Contains(line, "Reading configuration data") {
			t.Errorf("git's ssh in Git(%s) reads a configuration file: %s", dir, strings.TrimSpace(line))
		}
		k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
		if k == "identityfile" {
			ids = append(ids, v)
		}
		conf[k] = v
		if strings.Contains(samePath(v), home) {
			t.Errorf("git's ssh in Git(%s) reads %s %s, in the account's home", dir, k, v)
		}
	}
	if !slices.Equal(ids, []string{"none"}) || conf["identitiesonly"] != "yes" || conf["batchmode"] != "yes" ||
		samePath(conf["userknownhostsfile"]) != samePath(filepath.Join(dir, "known_hosts")) {
		t.Errorf("git's ssh in Git(%s) (%s) loads identity files %q, identitiesonly %q, batchmode %q and known hosts %q, want none, yes, yes and %s (git: %s)",
			dir, ssh, ids, conf["identitiesonly"], conf["batchmode"], conf["userknownhostsfile"], filepath.Join(dir, "known_hosts"), out)
	}
}

// samePath is p in one form for comparing the paths ssh prints: forward slashes, lower case, and an
// MSYS drive (/c/) as Windows writes it (c:/). Windows' ssh prints C:\Users\<user>/.ssh, and Git for
// Windows' ssh /c/Users/<user>/.ssh.
func samePath(p string) string {
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	if len(p) > 2 && p[0] == '/' && p[1] >= 'a' && p[1] <= 'z' && p[2] == '/' {
		p = p[1:2] + ":" + p[2:]
	}
	return p
}

// shellWords splits s as a POSIX shell splits a line of single-quoted words and backslashes, which
// is how Git quotes GIT_SSH_COMMAND, and git's shell reads it.
func shellWords(s string) []string {
	var words []string
	var w strings.Builder
	in, open := false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case in && c == '\'':
			in = false
		case in:
			w.WriteByte(c)
		case c == '\'':
			in, open = true, true
		case c == '\\' && i+1 < len(s):
			i++
			w.WriteByte(s[i])
			open = true
		case c == ' ':
			if open {
				words = append(words, w.String())
				w.Reset()
				open = false
			}
		default:
			w.WriteByte(c)
			open = true
		}
	}
	if open {
		words = append(words, w.String())
	}
	return words
}
