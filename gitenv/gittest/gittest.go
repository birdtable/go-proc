// Package gittest is the git a test runs, for tests only: a git child's environment (Git), the host's git
// (GitPath) and a repository of the test's own (Repo). It imports the standard library, gitenv and
// go-testkit, and only test files should import it.
package gittest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/birdtable/go-proc/gitenv"
	"github.com/birdtable/go-testkit"
)

// gitConfig is the variable that names git's global configuration file, which Git points into dir.
const gitConfig = "GIT_CONFIG_GLOBAL"

// who is the safe default identity a git child of Git commits under; one the caller appends wins.
func who() []string {
	return []string{"GIT_AUTHOR_NAME=testenv", "GIT_AUTHOR_EMAIL=testenv@example.invalid", "GIT_COMMITTER_NAME=testenv", "GIT_COMMITTER_EMAIL=testenv@example.invalid"}
}

// Git is the environment of a git child of a test: gitenv.Env of Clean of the test process's
// environment, so every name gitenv strips is gone (each GIT_ name in any case, and the variables
// that name a home), with no system configuration and no prompt for a person; then HOME as dir, a
// global configuration in the file gitconfig of dir, which a `git config --global` writes, and an
// identity to commit under. dir is the caller's, absolute, and none of the person's. With no HOME,
// libcurl reads the .netrc of the home the account's passwd entry names, and git sends its password
// to a host it names. dir holds none, so git sends none. Last, GIT_SSH_COMMAND as
// sshArgs, quoted for the shell git runs it in: on every system ssh takes ~ from the account (its
// passwd entry, or Windows' profile) whatever HOME says, and would read the person's ~/.ssh,
// its config, its known_hosts and a key with no passphrase. A test whose git wants a key adds its own. Then http.emptyAuth=false, as
// GIT_CONFIG_COUNT's entry 0, and KRB5CCNAME as a ticket cache in dir, which is never written: the
// person's Kerberos ticket, and on Windows their logon session through SSPI, is found with no file
// in HOME and no variable, and git from 2.55 on retries a host that offers Negotiate with an empty
// login so that libcurl sends it. emptyAuth=false stops that retry, and the empty cache keeps
// GSS-API from the person's ticket when the test's own .netrc gives git a login, which libcurl
// then spends on Negotiate. It also keeps the ticket from a proxy a test names, which libcurl answers
// with it given any proxy login, an empty one included, or http.proxyAuthMethod=negotiate. A caller
// that adds GIT_CONFIG_COUNT of its own replaces entry 0, so it counts from 1 and repeats this one.
// No environment stops a repository's own configuration: a child run in a checkout the test does not
// own reads its credential helpers and http headers, so a test runs it in a repository of its own.
func Git(dir string) []string { return gitFrom(testkit.Clean(os.Environ()), dir) }

func gitFrom(environ []string, dir string) []string {
	ssh := make([]string, 0, len(sshArgs(dir)))
	for _, a := range sshArgs(dir) {
		ssh = append(ssh, "'"+strings.ReplaceAll(a, "'", `'\''`)+"'")
	}
	return gitenv.Env(environ, slices.Concat([]string{"HOME=" + dir, gitConfig + "=" + filepath.Join(dir, "gitconfig")},
		who(), []string{"GIT_SSH_COMMAND=" + strings.Join(ssh, " ")}, noTicket(dir))...)
}

// noTicket is what keeps a git child of Git(dir) from authenticating as the person with Negotiate:
// no empty-login retry, and a Kerberos ticket cache in dir that holds nothing.
func noTicket(dir string) []string {
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.emptyAuth", "GIT_CONFIG_VALUE_0=false",
		"KRB5CCNAME=FILE:" + filepath.Join(dir, "krb5cc")}
}

// sshArgs is the ssh a test's git runs: no configuration file, no key, a known_hosts in dir and no
// prompt. ssh splits an option's value into words and reads tokens in it, so the path is in double
// quotes, with a backslash and a double quote escaped and a % doubled. ssh also reads ${NAME} there as
// an environment variable, which nothing escapes, so Git's caller keeps ${ out of dir: a test's
// name passes $, { and } to t.TempDir's folder.
func sshArgs(dir string) []string {
	hosts := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(filepath.Join(dir, "known_hosts"))
	return []string{"ssh", "-F", "none", "-o", "IdentitiesOnly=yes", "-o", "IdentityFile=none",
		"-o", `UserKnownHostsFile="` + hosts + `"`, "-o", "BatchMode=yes"}
}
