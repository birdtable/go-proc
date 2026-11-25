# sandbox

Run a program with the network closed, and its files held to folders if you like.

[![Go Reference](https://pkg.go.dev/badge/github.com/birdtable/go-proc/sandbox.svg)](https://pkg.go.dev/github.com/birdtable/go-proc/sandbox)

`sandbox` uses what the system gives: Landlock, never in its best-effort mode, and seccomp on
Linux, and the system sandbox, `sandbox-exec`, on macOS. It adds one block, the network closed and
files held to a few folders, set whole or refused where the system gives nothing. It never runs a
program with a weaker block than you asked for. You name the program, and for `RunHolding` the
folders it may use. Use it to run a tool you do not fully trust, such as a build or a test, without
letting it reach the network.

```
go get github.com/birdtable/go-proc/sandbox
CGO_ENABLED=0 go install github.com/birdtable/go-proc/sandbox/cmd/netblock@latest
```

From the command line:

```
netblock -- /usr/bin/make lint
netblock --read-write /work --read-only /usr -- /usr/bin/make lint
```

From Go. `Run` and `RunHolding` replace the process they are called in, so call them in a child you
start for the purpose:

```go
err := sandbox.Run("/usr/bin/make", []string{"lint"}, environ)
switch {
case errors.Is(err, sandbox.ErrUsage): // the program was not named by its absolute path
case errors.Is(err, sandbox.ErrNoBlock): // no block here: the program was not run
default: // the block was set, and the program could not start
}
```

Ask first, with no side effects:

```go
block, err := sandbox.Check(sandbox.Folders{}, "/usr/bin/make")
```

## What's inside

- `Run(path, args, environ)`: set the block on this process and exec the program. Files stay open.
- `RunHolding(folders, path, args, environ)`: the same, and every file outside the folders is
  denied, reads included. The program's loader and libraries are added read-only for you.
- `Check(folders, path)`: would the block be set here, and which `Block` is it? Sets nothing.
- `Block`: what the block closes (`Network`, `Daemons`, `Terminal`, `Signals`, `ServiceManager`).
  Decide by these fields, never by the system's name.
- `ErrNoBlock`, `ErrNoSystem`, `ErrUsage`: no block could be set (it matches `errors.ErrUnsupported`),
  this system gives none at all, and a program or folder the block does not take.
- [`cmd/netblock`](cmd/netblock): the same as a command. It exits 2 for a command line it does not
  take, 5 for a block it could not set, and 1 for a program that could not start behind a block.

## Systems

- **Linux 6.12 or later (amd64, arm64):** no privilege and no namespace. Landlock ABI 6 or later
  with erratum 2, a seccomp filter, and a probe before every run that proves the block holds. If
  any step is missing, the program is not run.
- **macOS:** the system sandbox, `sandbox-exec`, with a profile that denies the network.
- **Windows, FreeBSD and anything else:** no block, so the program is not run (`ErrNoSystem`).

## What it does not hold

Know these before you rely on it.

- **macOS is not containment.** launchd answers behind the profile, so a program can start a loaded
  agent of the account, which runs outside the sandbox with the network open. It can also signal a
  process outside. Apple marks `sandbox-exec` deprecated, and there is no probe at run time.
  `Check` says so: `Signals` and `ServiceManager` are false there.
- **A file something outside the block runs later** (a shell profile, a git hook beneath a
  read-write folder) is not protected.
- **Linux does not hold a file's metadata** (`stat`), and a descriptor open before the block, a
  standard stream among them, is used as before.
- **Libraries found only by `LD_LIBRARY_PATH`, `/etc/ld.so.cache` or `dlopen`** are not found.
  Name their folders yourself.

Say what you can prove: on Linux, the program runs with the network closed and its files held to
the folders you named. On macOS, it runs under the system sandbox with the network denied, and is
not contained. [docs/sandbox.md](../docs/sandbox.md) has the full list, with the probe, the daemons it
closes and the folders it refuses.

## Reference

Every exported name is documented at
[pkg.go.dev/github.com/birdtable/go-proc/sandbox](https://pkg.go.dev/github.com/birdtable/go-proc/sandbox).
