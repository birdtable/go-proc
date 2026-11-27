# sandbox: what it holds and what it does not

The reference for [`sandbox`](../sandbox). The package doc on
[pkg.go.dev](https://pkg.go.dev/github.com/birdtable/go-proc/sandbox) says how the block is set.

## What a caller may claim

On Linux 6.12 or later, the program runs with the network closed, and with RunHolding its files
held to the named folders and the folders its loader and libraries load from, proved by the probe
before each run. On macOS
the program runs under the system sandbox with the network denied, and is not contained. Elsewhere
the program is not run. A caller may not say a program is contained on macOS, nor that a file a
process outside the block reads later, a shell's profile or a git hook beneath a read-write folder,
is protected. A kernel at Landlock ABI 6 or later without erratum 2 is taken for one with no block,
since go-landlock takes it for ABI 5 and cannot make the ruleset with the scopes.

With RunHolding the program itself is held to the folders, as a file like any other. Its real file,
where its path ends once each symbolic link on it is followed, must be beneath a folder the caller
named or its loader's own folder, and on macOS so must each link on the way and the program that
runs it, if any: a #! script's interpreter, or /bin/sh for a file with neither #! nor a program's
format. RunHolding and Check walk the path before anything is set and refuse a program the
folders do not hold with ErrUsage, naming the folder it would need; they never add that folder,
since it would widen the block. The other load folders RunHolding adds come from the program's own
bytes, its run path and the libraries it names, so they never count toward holding the program: a
bundled tool whose $ORIGIN run path, or a library it names by its full path, would put its own
folder among them is refused unless the caller names that folder.

On Linux, the block is what enforces the folders: a program behind RunHolding reads and runs only
beneath the caller's folders, its loader's folder, and the real folders of the libraries the load
search vouched for, each beneath a folder the caller named or a built-in folder of the loader
(glibc's, by the loader's real folder: in its architecture's lib folder, /lib/<multiarch>,
/usr/lib/<multiarch>, /lib and /usr/lib; in lib64, /lib64 and /usr/lib64; in lib, /lib and
/usr/lib; musl's /lib, /usr/local/lib and /usr/lib). A library anywhere else fails at load, with
nothing granted for it. The real folder of a library beneath a built-in folder, a private folder in
/usr/lib/<multiarch> among them, is added, and all beneath it, so the claim above stays bounded by
the folders the loader and libraries load from. A library whose folder is / or /usr itself is
refused.

Apart from that, RunHolding and Check with folders refuse up front, with ErrUsage before anything
is set and naming the file and its folder, a library outside the caller's folders and the loader's
built-in folders, as the search finds it. That covers each library the program needs, and each one a
library it loads needs, by its own needs and run path, by its full path, by a run path, or through
a link in a built-in folder, and judged by its real file, so a link to one is judged by where it leads:
a library in a read-write folder that names /elsewhere/liby.so is refused as the program would be.
A caller that means such a library names its folder, or one above it. To the search a shared
library is any ET_DYN file without DF_1_PIE, so any such file on a run path counts as one.

The search imitates two loaders' search orders, so it refuses up front what it finds, not every
way a loader can reach a file. A library the loader reaches some other way is not promised
refused up front: one named by an audit or filter tag (DT_AUDIT, DT_DEPAUDIT, DT_FILTER or DT_AUXILIARY), one
in a glibc-hwcaps subfolder, and one found where a loader's search order differs from the
search's. Behind the block such a library loads only from the folders above, and from anywhere
else fails at load, with nothing granted for it.

The system sandbox holds every exec too, so the walk only says before the run what the kernel would
refuse after it. On Linux Landlock holds the exec of the real file. On macOS the profile denies
every exec but of a file beneath the folders and of /usr/bin/true, the dry run's program, which
reads nothing and exits 0, so a program behind the block starts no other program
outside them, a shell's command among them, and a link on the path swapped after the walk fails
closed, with the system sandbox's exit 71. There RunHolding reads a #! script's first line, with no
lookup, as the kernel reads it, to the first newline or #, and refuses a script whose interpreter
is outside the folders; a program env finds is held when env runs it. A file that may run, which
the kernel refuses with ENOEXEC and execvp runs with /bin/sh, is refused the same way unless /bin
is named: one with neither #! nor a program's format, a Mach-O file whose filetype is not
MH_EXECUTE, a library or an object, among them; a #! line with no end in its first 512 bytes, or
that names no interpreter; and a #! script whose interpreter is no Mach-O program, a script
itself or a plain file, as the kernel starts no other interpreter for a script. The kernel looks
that interpreter up under the system sandbox before it refuses it, so the interpreter must be
beneath the folders as well as /bin/sh, or the script is refused naming its folder. With /bin
named, such a file and a #!/bin/sh script run, but behind the block /bin/sh cannot read
/private/var/select/sh, the link that picks the shell it runs: it writes that error to the
program's standard error and runs its default shell. A file the caller may not run, which the
kernel refuses with EACCES before any shell or interpreter, is refused whatever its first line,
and so is a script whose interpreter the caller may not run, for which execvp runs no shell. The
kernel asks two things, and the refusal says which: the file's mode must have an execute bit, as
a file with none is refused whatever an ACL allows, and faccessat(2) with X_OK, by the effective
ids the exec runs with, must say yes, as the owner's bits decide for the owner, so a file of mode
0601 that the caller owns is refused.

## What the block does not close


  - On macOS launchd answers behind the profile: a process reaches it by its bootstrap port, so a
    program can start any loaded agent of the account, which runs outside the sandbox with the
    network open, and it can signal a process outside. Apple marks sandbox-exec deprecated, and
    macOS has no probe at run time: the dry run before each run shows only that the sandbox sets
    the profile. Check says so in its Block: Signals and ServiceManager are false there.
  - A file that a process outside the block runs later, such as a shell's profile or a git hook, is
    outside a network block on both systems. With no folder the block closes ways to the network,
    not writes; with folders, such a file can be written only beneath a read-write one.
  - Landlock does not hold a file's metadata (stat), and on both systems a descriptor open before
    the block, a standard stream among them, is read and written as before.
  - A folder on a file system whose files Landlock cannot match to the folder grants nothing: on
    Docker Desktop's share of the host's files (fakeowner) every file beneath a folder named there
    is refused. That fails closed.
  - A library only /etc/ld.so.cache, /etc/ld.so.conf, musl's /etc/ld-musl-<arch>.path,
    LD_LIBRARY_PATH, LD_PRELOAD or LD_AUDIT finds is not found, and neither is a library only in a glibc-hwcaps subfolder, or one a
    program opens by dlopen once it runs: its folder is the caller's to name. On macOS name the program by its
    real path, since a link on the way to it outside the folders (/var, /tmp and /etc are links)
    cannot be followed, and RunHolding refuses it with ErrUsage; a library outside dyld's shared
    cache, and the folder of each program it starts, /bin for a shell, are the caller's to name.
  - On macOS a universal file is taken for a program by its header, and its slices are not read.
    The kernel runs the slice it grades best for the Mac, and one that is not MH_EXECUTE it
    refuses with ENOEXEC, which execvp runs with /bin/sh, so a universal library passes Check and
    fails after the exec with exit 71 while /bin is not named. Telling the slice is grading the
    CPU types as the kernel does, and refusing every universal file would turn away the programs
    that run. Nor is a script's interpreter that the caller may not read told apart: it is held
    as named, and the kernel, which needs no read to run it, may still refuse it, closed.
  - The block takes no user namespace and no mount namespace, so a program sees every file's real
    owner, and it scrubs no *_SOCK or *_HOST variable from the environment: a variable only names a
    socket, which the block refuses.

## What a local daemon can do

A network block alone stops a fetch, not a process that asks a local
daemon to act for it: a container daemon's socket, the user's D-Bus bus (which can start a process
outside the block), a tmux server, launchd. On Linux, from Landlock ABI 9 a Unix socket on a path
made outside is refused by Landlock (EACCES) and one the program makes itself answers; on ABI 6
to 8 the filter refuses connect, sendmsg, sendmmsg and sendto with an address on every socket
(EPERM), so a program that starts a Unix server and dials it, as a test of a server does, fails
behind the block, and a socket pair or a socket handed down as a standard stream can be read and
written but not passed out by SCM_RIGHTS. On macOS every mach-lookup is denied but
com.apple.system.opendirectoryd.libinfo, which answers user and group lookups and starts nothing
(on a Mac bound to LDAP or Active Directory the directory service answers a lookup from that
server, so a program can send it a name it chooses: a channel to one server, not a fetch).

## The probe on Linux

Before the ruleset, the block makes two Unix sockets on a path in a folder under
the temporary folder, a stream that listens and a datagram socket, reaches both (the control), and
after the filter is set reaches them again: each must meet the block's own refusal, EACCES from ABI
9 and EPERM below, or the run is refused, since a host's own refusals (a socket's mode, an AppArmor
rule) are EACCES too and would hide a block that does not hold. It also makes an AF_INET socket,
which must be refused with the filter's EPERM on every ABI. With folders the probe's folder holds a
file as well, read and written before the ruleset and refused behind it. If the temporary folder is
beneath a folder the block grants, the run is refused, since the probe's refusal could not be
shown: name a temporary folder outside them in TMPDIR. RunHolding also needs /proc mounted on
Linux, since it names each folder to go-landlock by its descriptor's /proc/self/fd path; without
it the run is refused with ErrNoBlock, and Check with folders says so first.
