# service

Install, remove and query one login service or timer through the system's own tool.

[![Go Reference](https://pkg.go.dev/badge/github.com/birdtable/go-proc/service.svg)](https://pkg.go.dev/github.com/birdtable/go-proc/service)

`service` drives `systemctl --user` on Linux, `launchctl` in the login session on macOS and
`schtasks` on Windows, always in the user's scope. You write the unit, the plist or the task XML.
`service` writes the files, runs the tool, asks again, and refuses what a tool would misread,
adding the checks those tools leave out, and reads only their machine answers. It uses the standard library, and `golang.org/x/sys` on Windows.

```
go get github.com/birdtable/go-proc/service
```

```go
dirs, err := service.Folders() // where this system's tool reads units from
if err != nil {
	return err
}
p := service.Plan{Name: "app.timer", Files: []service.File{
	{Path: dirs[0] + "/app.service", Body: serviceBody},
	{Path: dirs[0] + "/app.timer", Body: timerBody},
}}
if err := service.Install(ctx, p, run); err != nil { // files written, unit taken, then asked
	return err
}
st, err := service.Query(ctx, p, run) // st.Installed, st.Enabled, st.Active if st.ActiveKnown
err = service.Remove(ctx, p, run)     // unit dropped, files removed, then asked
```

`run` is your `Runner`: it runs the tool and returns what it printed, its exit code, and an error
only if it could not run. You choose the environment, the time limit and the logging, and a test
hands in a fake.

## What's inside

- `Install`, `Remove`, `Query`: take, drop or report one unit. Installing twice leaves what once
  does, and removing what is not installed succeeds.
- `Plan`, `File`, `State`, `Runner`: the unit's name and files, what the tool answers, and how to run it.
- `Folders()`: the folders a plan's files may be in. Nothing is written anywhere else.
- `Quote(goos, s)`: one argument as that system reads it back.
- `ErrUsage`, `ErrPercent`, `ErrAbsent`: a plan refused before anything is written, a `%` in a
  Windows task, and a system with no tool. `ErrAbsent` matches `errors.ErrUnsupported`.

## Know the limits

- On Windows, Task Scheduler expands `%name%` and gives no way to keep a `%`, so `Install` refuses
  a task whose command, arguments or working folder hold one. Write the command's full path.
- Task Scheduler has no per-user namespace. A caller running as an administrator can replace or
  delete another account's task of the same name.
- `State.Active` is known on Linux only. On macOS and Windows `ActiveKnown` is false.
- A timer that must run while the person is logged out needs `loginctl enable-linger`. That is
  yours to tell them. The package does not do it.

[docs/service.md](../docs/service.md) has the full list, with what each call runs and each refusal.

## Systems

- **Linux:** `systemctl --user`.
- **macOS:** `launchctl` in `gui/<uid>`, with the plist in `~/Library/LaunchAgents`.
- **Windows:** `schtasks`, with the task's XML read once from a private temp folder.
- **Anything else:** every call returns `ErrAbsent`.

## Reference

Every exported name is documented at
[pkg.go.dev/github.com/birdtable/go-proc/service](https://pkg.go.dev/github.com/birdtable/go-proc/service).
