# service: what each call runs, what it refuses, what it does not hold

The reference for [`service`](../service). The package doc on
[pkg.go.dev](https://pkg.go.dev/github.com/birdtable/go-proc/service) has the API.

## What each call runs

Install on Linux is systemctl --user daemon-reload, then enable --now of the
name. On macOS it is launchctl bootstrap gui/<uid> of the plist when the label is not held, with a
bootout first when it is held and the plist changed. On Windows it is schtasks /create /tn <name>
/xml <file> /f. Remove undoes each, and each of Install and Remove ends by asking the tool again.
Query reads systemctl show -p LoadState,UnitFileState,ActiveState on Linux, launchctl print
gui/<uid>/<label> on macOS (exit 0 is held, 113 is not held) and schtasks /query /tn <name> /xml on
Windows (exit 1 is not installed). Only machine answers decide: exit codes, systemctl show's
properties and the task's XML. No word a tool prints for a person is read, so a Windows in another
display language and a reworded systemd answer the same. On any other system every call is ErrAbsent.

## What a plan is refused for

A plan is refused before anything is written or run, with an error that matches ErrUsage, when its
runner is nil; its name is empty, longer than 230 bytes, holds a capital or a character outside
lower-case ASCII letters, digits, '.', '-' and '_', starts with '-' or '.', ends in '.', or begins
(before its first '.') with a Windows device name; a file is not an absolute path in its clean
form or not in one of Folders(); on Windows its file has a Path; on macOS or Windows it has other
than one file; on macOS the plist's top Label is not the plan's name, or its file is marked UTF16;
on Linux a file holds a NUL, or a carriage return with no newline after it; or on macOS or Windows
the body declares a DOCTYPE with an internal subset. On Windows Install also refuses a task's XML
that does not parse, whose root element is not a Task, that declares an encoding other than UTF-8
or UTF-16, or whose Command, Arguments or WorkingDirectory hold a % (ErrPercent). The name rules
exist because the three tools read *, ? and [ as a pattern of many units (schtasks /delete /tn *
is every task on the computer), '-' as an option, '/' as a step of launchd's domain and '\' as a
folder of Task Scheduler, and because Task Scheduler and macOS's default file system read App as
app. On Linux and macOS each file is written only when its bytes differ, through a file beside it
renamed over it, with mode 0600, in a folder made 0700 if it is missing.

## What it does not hold


  - Another account's task on Windows. Task Scheduler has no per-user namespace: a name is the
    computer's, and /f overwrites and deletes without asking. The name rules keep a plan in the
    root folder, but a caller that runs as an administrator can still replace or delete another
    account's task of the same name there.
  - The folders above the task's folder on Windows. The task's folder is the user's alone and held
    (its owner and its only DACL entry are the user, its descriptor is read back, and the handle
    shares no delete), but schtasks reads task.xml by a path below the temp folder os.TempDir gives.
    An account that may rename or replace one of the folders above it can put another folder there
    before schtasks reads it. Keep the temp folder and the folders above it the user's. On a file
    system that keeps no ACLs (FAT, exFAT) the install is refused.
  - The program of a systemd command line. Quote("linux", s) is for an argument: the program comes
    first and is an absolute path, so it starts with '/' and never with one of systemd's prefix
    characters ('-', '@', '+', '!', ':').
  - A changed plist on a slow launchd. A changed plist is booted out and bootstrapped straight
    after; a launchd that has not finished the bootout refuses the bootstrap, and Install returns
    that refusal. The old plist is put back, so installing again tries again.
  - Active on macOS and Windows. Neither tool gives a machine answer for whether the program runs
    now, so State.ActiveKnown is false there.

What a person does that the install cannot do for them, such as loginctl enable-linger for a timer
that runs while they are logged out, is the caller's to list beside its Plan and show.
