# gitenv

An environment and arguments that point git at one folder, and nowhere else.

[![Go Reference](https://pkg.go.dev/badge/github.com/birdtable/go-proc/gitenv.svg)](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv)

A program that runs git inherits whatever its caller set: a `GIT_DIR` from a hook, a
`GIT_CONFIG_COUNT` that quietly turns a hook back on, a home with a `.netrc` in it. `gitenv` takes
those out and gives you the arguments that name the folder you mean, by git's own rules for its
environment variables and `-c` keys. You give it the environment to start from, and the child's
home: `Home`, `NoHome` or `NoGlobalConfig`. It uses the standard library only and starts no program.

```
go get github.com/birdtable/go-proc/gitenv
```

```go
cmd := exec.Command("git", gitenv.ReadArgs(dir, "status", "--porcelain")...)
cmd.Env = gitenv.Env(os.Environ())
out, err := cmd.Output()
```

## What's inside

- `Env(environ, add...)`: your environment without every `GIT_` name, the home variables and `NETRC`.
- `ReadArgs(dir, args...)`: `-C dir`, no optional lock, no file system monitor, no hook, then your arguments.
- `Repo(environ, add...)`: your environment without the variables that name a repository, for a child that writes.
- `Home(dir)` and `NoHome(empty)`: the `add` of a child whose home is `dir`, or that has no home of the person's.
- `NoGlobalConfig()`: the `add` of a child that keeps its home but reads no global configuration.
- `CommitMode(fi, fileMode, head)`: the mode git commits for a file you wrote, `100644` or `100755`.
- [`gitenv/gittest`](gittest): the same, ready for tests.

## Know the limits

`ReadArgs` turns off hooks and the file system monitor, and no other key. A folder you do not own
can still name a program through a filter, a diff or textconv driver, `gpg.program` or a remote's
`uploadpack`. Read such a folder through a clone you made yourself.
[docs/gitenv.md](../docs/gitenv.md) says what each read still runs.

`Env` keeps your `PAGER`, `EDITOR`, `VISUAL` and `SSH_ASKPASS`. A child with no `HOME` still finds
the account's own `.netrc`, and ssh still reads the person's `~/.ssh`. Add `NoHome`, and a
`GIT_SSH_COMMAND` of your own, when the child reaches a remote. The doc comments on `Env`,
`ReadArgs` and `NoHome` say how.

## Systems

Linux, macOS and Windows. `git init` writes the facts of the disk it is on (`core.filemode`,
`core.symlinks`, `core.ignorecase`), so a repository made on Windows differs from one made on Linux
or macOS. `CommitMode` reads `core.fileMode` so your commit agrees with git's own on every system.

## Reference

Every exported name is documented at
[pkg.go.dev/github.com/birdtable/go-proc/gitenv](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv).
