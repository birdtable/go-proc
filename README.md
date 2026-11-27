<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/mark/go-proc-banner-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/mark/go-proc-banner-light.svg">
    <img src="docs/mark/go-proc-banner-light.svg" width="300" alt="The go-proc mark, a process tree, its root in coral, then birdtable's coral bird in flight and the name.">
  </picture>
</p>

# go-proc

Run other programs from Go, with hard bounds and nothing left behind.

[![Latest Release](https://img.shields.io/github/v/release/birdtable/go-proc)](https://github.com/birdtable/go-proc/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/birdtable/go-proc.svg)](https://pkg.go.dev/github.com/birdtable/go-proc)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

go-proc starts a child program with a deadline, a cap on its output and an environment of only
what you give it. The child runs in a process tree that is killed whole when the run ends, so
nothing it started lingers. It's for tools, build systems and test suites that run programs they
don't fully trust to behave.

## Install

```sh
go get github.com/birdtable/go-proc
```

go-proc needs Go 1.27 or later.

## Quick start

Run `go version` for at most 30 seconds, keep at most 64 KiB of each stream, and give it a bare
environment:

```go
import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// Run a program with a deadline, a cap on what it may print and a bare environment. Run looks
// nothing up in PATH, so the caller finds the program first, here the go command.
func ExampleRun() {
	gocmd, err := exec.LookPath("go")
	if err != nil {
		fmt.Println(err)
		return
	}
	res, err := proc.Run(context.Background(), proc.Cmd{
		Path:      gocmd,
		Args:      []string{"version"},
		Timeout:   30 * time.Second,
		MaxOutput: 64 << 10,
		Drain:     2 * time.Second,
		Env:       proc.Bare(),
	})
	if ended, ok := errors.AsType[proc.Ended](err); ok {
		fmt.Println("the run was ended:", ended.Why)
		return
	}
	if err != nil {
		fmt.Println(err)
		return
	}
	fields := strings.Fields(string(res.Stdout))
	fmt.Println(res.Code, strings.Join(fields[:min(2, len(fields))], " "))
	// Output: 0 go version
}
```

`Timeout` and `MaxOutput` are required: a `Cmd` without them is refused before anything starts.
`Run` never goes through a shell and never looks a name up in `PATH`, so you pass the program's
path. When the run is ended early, the error is a `proc.Ended` that says why: `deadline`, `cap`,
`cancelled`, `escaped`, `unkillable` or `limit`.

## What's inside

- [`proc`](https://pkg.go.dev/github.com/birdtable/go-proc/proc): run a program with a deadline,
  an output cap and a bare environment, in a process tree killed whole when it ends. A child can
  get a fresh home of its own, and keep its cache folder between runs while the rest is fresh.
- [`sandbox`](https://pkg.go.dev/github.com/birdtable/go-proc/sandbox): run a program with the
  network closed and its files held to a few folders, or from the command line with
  `sandbox/cmd/netblock`.
- [`service`](https://pkg.go.dev/github.com/birdtable/go-proc/service): install, remove and query
  one login service or timer through the system's own tool.
- [`gitenv`](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv): an environment and arguments
  that point git at one folder, and nowhere else.
- [`gitenv/gittest`](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv/gittest): the git a
  test runs, for tests only.
- [`load`](https://pkg.go.dev/github.com/birdtable/go-proc/load): the system's load average, or
  `ErrNoAverage` where it keeps none, never a zero.

Each package's doc says what it is built on, what it adds and what your program configures, and
each package but `proc` has a README with a short tour; `proc`'s is this page.

### Built on

go-proc's direct dependencies, as `go.mod` requires them:

- [go-testkit](https://github.com/birdtable/go-testkit): helpers for go-proc's own tests and
  `gitenv/gittest`. A program that imports neither links none of it.
- [go-landlock](https://github.com/landlock-lsm/go-landlock): `sandbox`'s Landlock rules on Linux.
- [gopsutil](https://github.com/shirou/gopsutil): `load`'s averages on Linux.
- [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys): the system calls of `proc`, `sandbox`,
  `service` and `load`.
- [golang.org/x/term](https://pkg.go.dev/golang.org/x/term): `proc`'s check of whether `Stdin` is a
  terminal.

## A few more things `proc` does

- **Limits.** `Limits.TreeMemory` caps the memory of the whole tree, on Linux (in a cgroup systemd
  delegated) and on Windows. `Limits.Contained` also keeps a Linux tree from leaving that limit.
- **Peak memory.** `Cmd.ReadPeak` reports the most memory the run held.
- **Detached trees.** `proc.Start` starts a program in a tree that outlives the caller, and
  `proc.Interrupt` and `proc.Stop` ask it to stop and end it from any process.
- **Asking first.** `proc.Holds(cmd)` tells you whether this system would refuse a `Cmd`, without
  starting anything.

`proc` is a guard against accidents, not a jail. On Unix a child that leaves its process group is
not reached. On Windows a process that a system service starts on a child's behalf (WMI, Task
Scheduler) is not in the tree, and a process of the tree runs as your user, so it can duplicate the
job's handle: held open, the handle keeps the job, and that process, alive after your program
exits, and lets it allow breakaway, clear kill-on-close or lift the job's limits. A program killed
after Windows makes a child and before the next call puts it in its job leaves the child suspended.
The package docs say exactly what each bound holds.

## Systems

Linux, macOS and Windows, on amd64 and arm64, are built and vetted on every change. The tests run
on macOS (arm64), Linux (amd64) and Windows (amd64); arm64 on Linux and Windows is built and
vetted, not tested. For v0.19.0, the full release checks ran on macOS at the tagged commit, and
vet and the tests, with the race detector, ran on Linux, macOS and Windows at the tagged commit in
CI. No full release checks ran on Linux or Windows for this release.

Where a system can't hold what you ask, go-proc refuses with an error, before anything starts, and
never quietly gives you less. What each system refuses, by name:

| System | `proc` | `sandbox` | `service` | `load` |
|---|---|---|---|---|
| Linux | `Limits.Processes`; `Limits.TreeMemory` where no cgroup is delegated to it; `Limits.Contained` to a root caller, without `nsdelegate`, or where its shim can't take a step; `Usage`, `Pids` and `JoinTree` | a kernel older than 6.12, a processor other than amd64 or arm64, or Landlock turned off or without its erratum 2 | nothing | a `/proc/loadavg` of fewer than three fields, or one it can't read when `sysinfo(2)` fails too |
| macOS | `Limits.TreeMemory`, `Limits.Processes` and `Limits.Contained`; `Cmd.File`; `Usage`, `Pids` and `JoinTree` | a `sandbox-exec` that cannot start or set its profile | nothing | an unreadable `vm.loadavg` |
| Windows | `Limits.Contained`; `Cmd.File`; `ChildrenCPU`; `Interrupt` of a live tree; a `Grace` without `Terminal` and a console; a `Path` with a drive and no root, or a root and no drive; a file that is not an `.exe` or `.com` | every run | a task that holds a `%` | every read |
| FreeBSD and other Unix | as macOS, and `Start`, `Stop` and `Interrupt` | every run | every call | every read |

A refused `Run` wraps `ErrRefused`, and a call this system has no way to make wraps
`errors.ErrUnsupported`; `sandbox`'s refusals match `ErrNoBlock`, `service`'s `ErrAbsent` (a
Windows task that holds a `%` matches `ErrPercent` instead, and neither of the others) and
`load`'s `ErrNoAverage`. `gitenv` refuses nothing by system. The package docs say exactly what each
bound holds on each system.

## Stability

v0: the API may change between minor versions. [`api/contract.txt`](api/contract.txt) lists what a
program may rely on, one promise a line, each held by a test.

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) says how to run the tests.

## Reporting issues

Report a bug or ask for a feature through the [issue forms](https://github.com/birdtable/go-proc/issues/new/choose).
They are read as time allows, and no answer time is promised. A security problem is never an
issue: report it privately, as [SECURITY.md](SECURITY.md) says.

## Security

Please don't report a vulnerability in a public issue. [SECURITY.md](SECURITY.md) says how to
report one privately.

## License

[Apache-2.0](LICENSE). See also [NOTICE](NOTICE).
