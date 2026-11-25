// Package gitenv gives git an environment and arguments that name one folder, built on git's own
// rules for its variables and -c keys. It adds what git leaves to its caller: no variable of the
// caller's that points git at another repository, work tree, index, object store or configuration,
// and no hook and no file system monitor from the folder's own configuration. It imports the
// standard library only and starts no program.
//
// ReadArgs turns off those two keys, core.hooksPath and core.fsmonitor, and no other, so a folder's
// own configuration can still name a program (a filter, a diff or textconv program, gpg.program, an
// uploadpack or ssh command): https://github.com/birdtable/go-proc/blob/main/docs/gitenv.md says which.
//
// A program that runs git as a child of its own takes the environment it was started in, and a
// hook, a harness or a person has set some of it. Left in, GIT_DIR makes every folder answer as one
// repository, and GIT_CONFIG_COUNT with a key and a value can set core.hooksPath or core.fsmonitor
// for the child, which then runs a program the caller never named. Env takes them out
// and ReadArgs puts the folder in, and the two are for a child that reads. Repo takes out only the
// variables that name a repository, for a child that writes under the caller's own author. Neither
// reads the process's own environment: the app gives each the environment to start from.
// Home and NoHome are what a caller adds after Env for its child's home: a folder of the caller's,
// or no home of the person's, so git reads no global configuration and libcurl no .netrc.
// NoGlobalConfig is the add of a child that keeps its home but reads no global configuration.
// CommitMode is the mode git commits for a regular file the caller wrote, by git's own rule.
//
// git init probes the disk a repository is made on and writes what it finds into that repository's
// configuration (core.filemode, core.symlinks, core.ignorecase, and on macOS core.precomposeunicode).
// Each states a fact of the disk, not a choice, so gitenv pins none; docs/gitenv.md lists each system's.
package gitenv

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

// names is every environment variable git reads to find a repository, its work tree, index or
// object store, or its configuration, other than the folder it runs in: what git itself lists under
// rev-parse --local-env-vars, which a test holds this list to. Repo drops these and Env drops these
// and every other name that starts GIT_. The list does not hold the names GIT_CONFIG_KEY_<n> and
// GIT_CONFIG_VALUE_<n>, which a GIT_CONFIG_COUNT counts: dropping GIT_CONFIG_COUNT is what takes them
// out, and Env drops them too.
func names() []string {
	return []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_COMMON_DIR",
		"GIT_INDEX_FILE", "GIT_PREFIX", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
		"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE",
	}
}

// homeNames is every variable that names a folder or file of the person's own where git reads its
// global configuration or libcurl a password: the home folder, Windows' own names for it, the
// configuration folder, and NETRC, the .netrc that libcurl reads ahead of the home's from 8.16 on.
func homeNames() []string {
	return []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "XDG_CONFIG_HOME", "NETRC"}
}

// Env is environ without every variable that moves the repository or its configuration: each name
// that starts GIT_ (the names of names, GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM among them, and
// each GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n>), whatever its case, and the variables that name
// the person's home, where the global configuration is, and NETRC, which names a .netrc. It then
// sets two safe defaults: GIT_CONFIG_NOSYSTEM=1, no system configuration, and GIT_TERMINAL_PROMPT=0,
// no wait for a person, and then add, each one NAME=value, which is the caller's own and so comes
// last and wins: a caller that wants a home of its own adds Home or NoHome. Everything else, PATH
// among it, is kept, the caller's PAGER, EDITOR, VISUAL and SSH_ASKPASS too: GIT_TERMINAL_PROMPT=0
// does not stop SSH_ASKPASS on an authentication prompt.
//
// A child with no HOME that fetches or pushes over http or https sends the password of the .netrc in
// the home the account's passwd entry names to a host it names, as libcurl reads it there: a caller
// whose child reaches a remote so adds NoHome, or a Home that holds no .netrc. Env drops NETRC for the
// same reason, as libcurl 8.16 and later reads the file it names ahead of HOME's .netrc. Over ssh,
// HOME does not help: on every system ssh takes ~ from the account, its passwd entry or Windows'
// profile, and reads the person's ~/.ssh, its config and keys among it, so a caller adds a
// GIT_SSH_COMMAND that names its own (-F none, IdentityFile=none and a UserKnownHostsFile of its own),
// as gittest.Git does; a test whose git wants a key names one there.
func Env(environ []string, add ...string) []string {
	out := make([]string, 0, len(environ)+2+len(add))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if movesGit(name) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return append(out, add...)
}

// Repo is environ without the variables that name a repository, its work tree, index, object store or
// configuration (names, whatever their case), then add, each one NAME=value, which is the caller's
// own and so comes last and wins. It keeps the rest of GIT_ (the author and the committer a commit is
// made as) and everything else, so it is the environment of a child that writes in a folder of its
// own and must not write in the one a hook named. Repo never reads the process's environment.
func Repo(environ []string, add ...string) []string {
	out := make([]string, 0, len(environ)+len(add))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(names(), strings.ToUpper(name)) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, add...)
}

// movesGit is whether the variable named name is one Env drops, as the system names variables: a
// name starts GIT_ in any case, and each of homeNames is the same name in any case.
func movesGit(name string) bool {
	upper := strings.ToUpper(name)
	return strings.HasPrefix(upper, "GIT_") || slices.Contains(homeNames(), upper)
}

// ReadArgs is the arguments of a git that reads the folder dir and nothing else: git runs in dir,
// takes no optional lock (so a read leaves the index alone), runs no file system monitor and runs no
// hook, whatever the folder's own configuration says, and then args, the subcommand and its own.
// The path of no hook folder is /dev/null, which git reads as an empty one on every system it runs on.
// A folder's own configuration can still name a program a read runs (docs/gitenv.md says which), so
// a caller that reads a folder it does not own reads a clone it made itself.
func ReadArgs(dir string, args ...string) []string {
	out := []string{"-C", dir, "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}
	return append(out, args...)
}

// The refusals of Home and NoHome, each wrapped with the folder it refuses.
var (
	errNoFolder = errors.New("gitenv: the home folder is empty, and an empty HOME names no folder a child can be held to")
	errRelative = errors.New("gitenv: the home folder is relative, so it names a folder by the child's working directory")
	errUnclean  = errors.New("gitenv: the home folder is not clean, so it is not the one filepath.Clean names")
)

// Home is the add of Env for a child whose home is the folder dir: HOME=dir. git for Windows reads
// HOME ahead of HOMEDRIVE, HOMEPATH and USERPROFILE, and libcurl reads it ahead of the account's
// passwd entry, so on every system the child's global configuration and .netrc are dir's, and Env
// has already dropped XDG_CONFIG_HOME and NETRC. It refuses a dir that is empty, not absolute as
// filepath.IsAbs tells on the running system, or not clean (filepath.Clean(dir) != dir), with an
// error naming which. It does no I/O: that dir is a folder, and what it holds, are the caller's.
func Home(dir string) ([]string, error) {
	if err := folder(dir); err != nil {
		return nil, err
	}
	return []string{"HOME=" + dir}, nil
}

// NoHome is the add of Env for a child with no home of the person's: HOME=empty and
// GIT_CONFIG_GLOBAL=/dev/null. empty is a folder of the account's alone that holds nothing, which the
// caller makes, with go-files' MkdirFresh or MkdirPrivate (v0.10.0): NoHome does no I/O and checks
// neither, and refuses empty as Home refuses its dir. git reads no global configuration from
// /dev/null, that exact string on every system, as git for Windows maps it to its own null device;
// GIT_CONFIG_GLOBAL is read from git 2.32 on, and below it the empty folder is what holds. libcurl
// finds no .netrc in empty. NoHome sets no NETRC, as libcurl on Windows would take /dev/null for a
// path anyone may make at a drive's root; a NETRC the caller puts in add itself is its own choice, and
// NoHome does not undo it. Over ssh, HOME does not help: ssh takes ~ from the account, so a caller adds
// a GIT_SSH_COMMAND that names its own config, keys and known hosts, as Env's doc says.
func NoHome(empty string) ([]string, error) {
	if err := folder(empty); err != nil {
		return nil, err
	}
	return append([]string{"HOME=" + empty}, NoGlobalConfig()...), nil
}

// NoGlobalConfig is the add of Env for a child that keeps a home but reads no global configuration:
// GIT_CONFIG_GLOBAL=/dev/null, NoHome's second element. Env's result comes first and this add after
// it, after the caller's own add too, as Env drops every GIT_ name of the environment it is given.
// /dev/null is that exact string on every system, as git for Windows maps it to its own null device.
// git reads GIT_CONFIG_GLOBAL from 2.32 on; below that it reads the home's configuration. Env
// already sets GIT_CONFIG_NOSYSTEM=1. The home stays the caller's, so libcurl still reads a .netrc
// there; NoHome is the add of a child that is to read neither. Each call gives a new slice.
func NoGlobalConfig() []string {
	return []string{"GIT_CONFIG_GLOBAL=/dev/null"}
}

// folder refuses a dir that is empty, relative or not clean, in that order.
func folder(dir string) error {
	switch {
	case dir == "":
		return errNoFolder
	case !filepath.IsAbs(dir):
		return fmt.Errorf("%w: %q", errRelative, dir)
	case filepath.Clean(dir) != dir:
		return fmt.Errorf("%w: %q is %q", errUnclean, dir, filepath.Clean(dir))
	}
	return nil
}

var (
	errNotRegular = errors.New("gitenv: the file is no regular file, so git commits no 100644 or 100755 for it")
	errHeadMode   = errors.New("gitenv: HEAD's mode for the path is none of \"\", 100644 and 100755")
)

// CommitMode is the mode git commits for a regular file a program wrote, 100644 or 100755, as git's
// own rule (ce_mode_from_stat) decides it on every system. fi is the file's own FileInfo, from an
// Lstat; fileMode is the repository's core.fileMode; head is HEAD's mode for the path, "" for a new
// path. With fileMode true the mode is the owner's exec bit, whoever runs; with it false it is head,
// or 100644 for a new path. Git for Windows sets core.fileMode false in every repository it makes,
// and Go reports no exec bit there, so the owner's bit is never what decides it on Windows.
//
// The caller reads core.fileMode, as `git config --type=bool --get core.fileMode` in the repository,
// started with ReadArgs and Env: true or false, and absent (git exits 1 and prints nothing) means
// true, git's default. git reads the index's mode where this reads head, and the two are one for a
// caller that stages the file against HEAD.
//
// It refuses a fi that is nil or no regular file (a folder, a link, a device), and a head that is
// none of "", 100644 and 100755, 120000 (a link) among them, as a file written at a path never
// replaces a link or a submodule there. It does no I/O.
func CommitMode(fi fs.FileInfo, fileMode bool, head string) (string, error) {
	switch {
	case fi == nil:
		return "", errNotRegular
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%w: %q is %v", errNotRegular, fi.Name(), fi.Mode().Type())
	case head != "" && head != modeFile && head != modeExec:
		return "", fmt.Errorf("%w: %q", errHeadMode, head)
	case fileMode && fi.Mode().Perm()&0o100 != 0:
		return modeExec, nil
	case fileMode, head == "":
		return modeFile, nil
	}
	return head, nil
}

// The two modes git commits for a regular file.
const (
	modeFile = "100644"
	modeExec = "100755"
)
