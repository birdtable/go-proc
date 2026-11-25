# gitenv: what git still runs, and what git init writes

The reference for [`gitenv`](../gitenv). The package doc on
[pkg.go.dev](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv) has the API.

## What a read still runs


ReadArgs turns off the hooks and the file system monitor and no other key, so it does not stop a
folder's own configuration from naming a program. These reads still run what the folder names:
git status (a filter.<driver>.clean program, when a file's stat has changed), git diff
(diff.external, and a diff.<driver>.textconv program even with --no-ext-diff), git blame (textconv),
git log (gpg.program, when log.showSignature is true), and git ls-remote and git fetch
(remote.<name>.uploadpack on a local remote, core.sshCommand on an ssh one). A key with a fixed name
can be pinned off with -c, but a filter, diff or remote is named by the folder, so no list of pins
holds them all and none is offered. A caller that reads a folder it does not own reads a clone it
made itself: a clone copies neither the configuration nor the hooks, so a driver that
.gitattributes names has no program behind it.

## What git init writes on each system

Each states a fact of the disk, not a choice, so gitenv pins none. Linux on its own disk writes
core.filemode = true; macOS (APFS) writes core.filemode = true, core.ignorecase = true and
core.precomposeunicode = true; a Linux git on a case-insensitive mount writes core.ignorecase = true
too; Windows on NTFS writes core.filemode = false, core.symlinks = false and core.ignorecase = true.
A test that commits the same files on each system finds the bytes of Linux and macOS the same, and
Windows different in two modes (a link is stored as a file, and an executable has no executable
bit), so in the commit's names.
