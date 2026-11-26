# Security

go-proc runs other programs and holds them to bounds: a deadline, an output cap, a process tree
killed whole, memory limits, a closed network. A way around one of those bounds is a security
problem, and we want to hear about it.

## Reporting a vulnerability

Please report it privately, through GitHub's private vulnerability reporting: open the
repository's **Security** tab and choose **Report a vulnerability**, or go straight to
<https://github.com/birdtable/go-proc/security/advisories/new>.

Please don't open a public issue or pull request for a vulnerability.

A helpful report says:

- the go-proc version (`go list -m github.com/birdtable/go-proc`), the Go version and the system;
- which package and call, what it should hold and what a program can reach instead;
- what the system, the caller or the program must have or do for it to happen.

We'll answer in the advisory, work on a fix there with you, and credit you when it's published,
if you'd like.

## Supported versions

go-proc is at v0, and fixes go into the latest minor version only.

| Version | Supported |
|---|---|
| latest minor (v0.x) | yes |
| older | no |

## What is not a vulnerability

Each package's docs say what it does not hold. For example, `proc` is a guard against accidents,
not a jail. These are documented, and not a vulnerability on their own:

- on Unix, a child that leaves its process group;
- on Windows, a process that a system service starts on a child's behalf (WMI, Task Scheduler);
- on Windows, a process of the tree that duplicates its job's handle from the caller, which keeps
  the job, and itself, alive after the caller exits, and lets it allow breakaway, clear
  kill-on-close or lift the job's limits;
- on Windows, a process that `Run` or `Start` made, left suspended with no parent and in no job that
  ends it, when the caller is killed after Windows makes the process, inside `os/exec`'s start, and
  before the next call puts it in its job. Measured on GitHub's `windows-latest` runner with the race
  detector, that window is about 5 ms of a 6 ms start: of 100 callers each of `Start` and of `Run`
  killed 0 to 9.9 ms after they began, 43 and 46 left a process so, every one killed 1.1 to 6.1 ms in.
  No order of calls after `os/exec`'s start can shrink it. A process left so runs nothing, and
  `taskkill /f /pid <pid>` ends it, as does the end of the session it runs in. A start of go-proc's
  own, which makes the process in its job, would close the window; it changes `Tree.Leader`'s type,
  and is left to the API review before v1.0.0.

If you find a way around a bound the docs say *is* held, that is one.
