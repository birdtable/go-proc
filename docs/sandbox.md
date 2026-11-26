# sandbox: what it holds and what it does not

The reference for [`sandbox`](../sandbox). The package doc on
[pkg.go.dev](https://pkg.go.dev/github.com/birdtable/go-proc/sandbox) says how the block is set.

## What a caller may claim

On Linux 6.12 or later, the program runs with the network closed, and
with RunHolding its files held to the named folders, proved by the probe before each run. On macOS
the program runs under the system sandbox with the network denied, and is not contained. Elsewhere
the program is not run. A caller may not say a program is contained on macOS, nor that a file a
process outside the block reads later, a shell's profile or a git hook beneath a read-write folder,
is protected. A kernel at Landlock ABI 6 or later without erratum 2 is taken for one with no block,
since go-landlock takes it for ABI 5 and cannot make the ruleset with the scopes.

With RunHolding the program itself is held to the folders, as a file like any other. Its real file,
where its path ends once each symbolic link on it is followed, must be beneath a folder held, the
load folders RunHolding adds included, and on macOS so must each link on the way. RunHolding and
Check walk the path before anything is set and refuse a program the folders do not hold with
ErrUsage, naming the folder it would need; they never add that folder, since it would widen the
block. On Linux Landlock holds the exec of the real file too, so the walk only says before the run
what the kernel would refuse after it. On macOS the system sandbox does not hold a program's exec:
a program in a folder no profile grants runs. There the hold of the program is RunHolding's own
check, of the path as it walked it, and a link on the path swapped after the walk, by the program's
owner or one who can write its folders, moves the program out of the folders unseen, as a file
swapped after it was read moves a load folder. On macOS a #! script is not refused, and the hold
covers the script's own file and the links to it, not the interpreter its first line names: RunHolding
does not read that line there, and the system sandbox does not hold that exec either.

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
    LD_LIBRARY_PATH, LD_PRELOAD or LD_AUDIT finds is not found, and neither is one a program opens
    by dlopen once it runs: its folder is the caller's to name. On macOS name the program by its
    real path, since a link on the way to it outside the folders (/var, /tmp and /etc are links)
    cannot be followed, and RunHolding refuses it with ErrUsage; a library outside dyld's shared
    cache is the caller's to name.
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
