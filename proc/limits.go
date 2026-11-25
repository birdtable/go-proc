package proc

import (
	"errors"
	"fmt"
	"math"
)

// Limits are bounds a run's tree is held to while it runs, each refused by name, before anything
// starts, where this system cannot hold it, never weakened. The zero Limits holds the run to none.
type Limits struct {
	// TreeMemory is the most memory, in bytes, the whole tree may hold together; 0 is no limit.
	// Windows holds it on the tree's Job Object, as the memory committed to the whole tree, of which
	// PeakProcessCommitted is one process's, on a program that does not lift it: a process of the
	// tree runs as the caller's user, at its integrity, so it may open the caller, duplicate the
	// job's handle from it and clear the limit, and is then held to nothing. Linux
	// holds it as memory.max, with no swap, the charge PeakTreeCharged counts, of a cgroup of the
	// run's own beside this process's in the one systemd delegated: the tree starts in it, and the
	// kernel kills it whole past the limit. systemd delegates so to a service, or a program started by
	// systemd-run --user, with Delegate=yes and DelegateSubgroup=app (systemd 254 on); not to a login
	// shell's scope, nor by systemd-run --user --scope, whose caller runs in the delegated scope itself,
	// its parent with no user.delegate mark. A hugetlb page is charged to neither it nor Peak unless
	// cgroup v2 has memory_hugetlb_accounting (Linux 6.7). The files are the caller's user's, so a
	// process of the tree may write max to memory.max or memory.swap.max, or 0 to memory.oom.group,
	// and is held to less or nothing; or write its pid to another delegated cgroup's cgroup.procs,
	// the caller's among them, and leave the limit, the end's kill and the Peak: Contained closes
	// those. Linux refuses it where no cgroup is delegated so, or from within a run's cgroup, as a
	// program a run of proc runs is; so does every other system.
	TreeMemory uint64
	// Contained holds the tree to TreeMemory against a move or a write of its own processes, as far
	// as stated here, and is refused without a TreeMemory, with Terminal (the tree would share the
	// caller's controlling terminal, and by TIOCSTI type at it an INTR that signals the caller's
	// group, or a line the user's shell runs once the caller exits), and on Linux to a caller whose
	// effective uid is 0 (the tree would keep uid 0, and with it the kernel's global sysctls,
	// kernel.core_pattern among them, which run a helper as root outside the tree: a caller takes
	// another uid to contain a run). A caller whose effective gid is 0, or with group 0 among its
	// supplementary groups, is not refused: the tree keeps group 0, and with it the group bits of
	// every file and global sysctl of group 0. Linux starts the program in a user namespace that
	// maps the caller's effective uid and gid each to itself and no other, with a cgroup namespace
	// rooted at the run's cgroup and pid and mount namespaces of its own, through a shim: the
	// caller's binary run again, which writes 0 to user.max_cgroup_namespaces there, so no
	// process of the tree nests a cgroup namespace, makes the tree's mounts private, mounts a proc
	// of the tree's own on /proc and a cgroup v2 of its cgroup namespace, rooted at the run's
	// cgroup, over each cgroup mount, enters the program's folder, its Dir or the caller's own, then
	// takes it again by its path through them (the start itself enters none, so its errno is never the Cmd's), closes at the
	// exec every descriptor of the caller's the run does not hand the tree (Stdin, Stdout, Stderr,
	// ExtraFiles and File's), refuses a directory among those it does, or one it cannot check, sets no_new_privs, drops every
	// capability and execs the program; a shim that cannot is refused by name before the program
	// starts. So no path of the tree's own names a cgroup outside the run's: not the caller's, not
	// its parent's, not another run's. A run whose folder, its Dir or the caller's
	// own, the shim cannot enter by its path (one in a cgroup mount, one through a proc link, /proc/self/cwd or /proc/self/fd/N, to a folder there or on no mount of the tree's, one removed, one the caller's
	// uid cannot enter or one below a folder it cannot search), or that hands the tree a directory, as its Stdin or
	// among its ExtraFiles, from which ".." walks the caller's mounts, or a descriptor whose fstat
	// fails (on a FUSE mount whose server died, say), is refused, wrapping
	// ErrRefused alone: the run, not the system, is the cause. With cgroup v2 mounted with nsdelegate (else Contained is
	// refused), the kernel refuses the tree a move across the run's cgroup and a write of its files
	// but cgroup.procs, cgroup.threads, cgroup.subtree_control, memory.oom.group and memory.reclaim.
	// While this process runs, it kills the tree by cgroup.kill once memory.events counts an oom, so
	// a write of 0 to memory.oom.group ends it all the same; and, by one inotify instance, once a
	// write of -memory to the parent's cgroup.subtree_control, by another process of the caller's
	// user, takes memory from the cgroup, its memory.max reads other than set, or what the run reads
	// cannot be: the run then ends with an error that names the loss, never limit. What it costs the program: it sees
	// its cgroup as /, where its Go runtime reads the run's own cpu.max, which the run leaves unset, so no CPU limit, its own runs of proc are refused
	// TreeMemory, and it makes no cgroup namespace; it is pid 1, so it ignores a signal it does not
	// handle (TERM among them; Grace's KILL still ends it), reaps its tree's orphans, and ends the
	// tree as it exits; it sees every uid and gid but the caller's two as 65534, its access
	// unchanged; no setuid file or file capability gives it anything; the shim's memory counts in
	// TreeMemory, and so does the shim's last image, the caller's own binary, which the kernel frees only
	// after the program's exec: the run's cgroup is charged it as the program starts, for a moment, about
	// 13 MiB for a plain go-proc test binary and about 48 MiB for a race-built one, so a TreeMemory has to
	// leave that room beside what the program holds, and one below it ends the run by limit before the
	// program starts, as the Timeout or ctx ending there ends it by deadline or cancelled, and the caller's other init
	// functions run in it, with no environment: one that ends it before its steps refuses the run,
	// wrapping ErrRefused, as does a start that skips the shim, which only an init function of
	// the caller's own binary can do. A program of that start that writes the shim's ready byte to the
	// report it inherits fails the check made when the report says ready, which reads each thread
	// of the run's cgroup for no_new_privs and no capability, as the shim leaves them, and its tree is
	// killed; one that never writes the byte runs unheld to its end. Each is refused after the start,
	// and its output is not given. The check reads each thread's mounts too, by mount id and parent id, for the shim's cover over each
	// cgroup mount, with the cover's root the run's cgroup as the caller shows it, so a program that dropped
	// its capabilities and set no_new_privs itself before it wrote the byte, and left a cgroup mount open,
	// is refused and its tree killed. It refuses a descriptor of any thread on a cgroup file system or a mount namespace, and does not see one
	// a process of the user's outside the tree holds and hands to the program after the byte. It reads back
	// the files the run wrote to its own cgroup, memory.max, memory.swap.max and memory.oom.group,
	// and does not see a write before the byte to a cgroup other than the run's, which survives the
	// refusal and the kill. A second process of the same user, outside the run's cgroup namespace, can
	// move a process of the run to another cgroup of the user's: that process is charged there, so
	// the run's memory limit does not hold for it while the move lasts, and the tree's kill reaches it
	// through the run's pid namespace. A start that skipped the shim can also keep a reference to the
	// caller's cgroup mount that is no descriptor on a cgroup file system, as a thread's working directory
	// or root, a clone of a mount above the cgroup one, or a descriptor in flight in a socket pair the tree
	// holds both ends of, none of which the check reads. The run's cgroup is frozen across the check, and a cgroup that cannot be
	// frozen refuses the run. The shim starts the program only once the check has passed, so the check
	// reads no step of the program's. Where the kernel, a security module or a container refuses an unprivileged user namespace or the proc mount, the run is
	// refused, wrapping ErrRefused and errors.ErrUnsupported, a security module's EACCES among them,
	// as is a run whose shim cannot cover the cgroup mounts, as in a root that is not a mount point. Linux does not hold, where Yama is
	// absent or its ptrace_scope is 0, the memory of a process of the caller's uid by its files in a
	// proc mount of an outer pid namespace the host shows elsewhere than /proc, by whose root the
	// tree reaches the host's cgroup mount; nor an open file of a cgroup outside the run's, its
	// cgroup.freeze say, that the caller hands the tree. A pidfd the tree gets for a process outside
	// it, as by SO_PEERPIDFD over a unix socket it connects to, gives it none of that process's
	// descriptors: the kernel refuses pidfd_getfd from a user namespace below the process's, whatever
	// Yama says. Nor does Linux hold a process that acts for the tree outside it, as the user's
	// service manager asked to freeze or kill a unit. Windows refuses it, as a process of the tree may
	// clear its job's limit.
	Contained bool
	// Processes is the most processes the tree may hold at once, the program among them; 0 is no
	// limit, and below 0 is refused. Only Windows holds it, on a program that does not lift it, as
	// TreeMemory says; elsewhere it is refused.
	Processes int
}

// limitKind is which limits a system holds.
type limitKind int

const (
	// limNone holds no limit.
	limNone limitKind = iota
	// limCgroup is Linux: TreeMemory, on a cgroup of the run's own; no Processes.
	limCgroup
	// limJob is Windows: TreeMemory and Processes, on the tree's Job Object.
	limJob
)

// The refusals of Limits.
var (
	errProcesses = fmt.Errorf("%w: Limits.Processes is negative", ErrRefused)
	// errContainedAlone is a Contained with no TreeMemory to hold the tree to.
	errContainedAlone = fmt.Errorf("%w: Limits.Contained holds the tree to Limits.TreeMemory, which is 0", ErrRefused)
	// errContainedTerminal is a Contained with Terminal, whose tree would share this process's
	// controlling terminal, and by TIOCSTI type at it as its user.
	errContainedTerminal = fmt.Errorf("%w: Limits.Contained with Terminal: the tree would share this process's controlling terminal, and could type at it, as the user, an INTR that signals this process's group or a line that this process's shell runs outside the tree", ErrRefused)
	// errContainedDir is a Contained whose Dir is no folder, checked before the start, which would
	// fail on it as on a host that refuses the run's namespaces.
	errContainedDir = fmt.Errorf("%w: Limits.Contained: the run's Dir names no folder", ErrRefused)
	// errNoTreeMemory, errNoProcessLimit and errNoContained are a limit this system cannot hold.
	errNoTreeMemory   = notHeld{fmt.Errorf("%w: this system gives the runner no way here to hold a run's tree to Limits.TreeMemory, and a limit is never dropped: %w", ErrRefused, errors.ErrUnsupported)}
	errNoProcessLimit = notHeld{fmt.Errorf("%w: this system gives the runner no way here to hold a run's tree to Limits.Processes, and a limit is never dropped: %w", ErrRefused, errors.ErrUnsupported)}
	errNoContained    = notHeld{fmt.Errorf("%w: this system gives the runner no way here to hold a run's tree to Limits.Contained, which is never dropped: %w", ErrRefused, errors.ErrUnsupported)}
	// errTreeMemoryWide and errProcessesWide are a limit wider than the field that holds it.
	errTreeMemoryWide = fmt.Errorf("%w: Limits.TreeMemory is more than this system's memory limit holds", ErrRefused)
	errProcessesWide  = fmt.Errorf("%w: Limits.Processes is more than a Job Object's process limit holds", ErrRefused)
)

// limitsRefused is why l is not held on a system of kind k, or nil. A field the system does not hold
// is refused by its name, before a field it holds is checked. On Linux whether a cgroup is delegated
// here is asked after, by the run (cgroupFor).
func limitsRefused(l Limits, k limitKind) error {
	switch {
	case l.Processes < 0:
		return errProcesses
	case l.Contained && l.TreeMemory == 0:
		return errContainedAlone
	case l == Limits{}:
		return nil
	}
	switch k {
	case limNone:
		if l.TreeMemory > 0 {
			return errNoTreeMemory
		}
		return errNoProcessLimit
	case limCgroup:
		switch {
		case l.Processes > 0:
			return errNoProcessLimit
		case l.TreeMemory > math.MaxInt64:
			// memory.max is a signed 64-bit count of bytes.
			return errTreeMemoryWide
		}
		return nil
	case limJob:
		if l.Contained {
			// A process of the tree may clear the job's limit by a handle it duplicates.
			return errNoContained
		}
		return jobRefused(l, uint64(^uintptr(0)))
	}
	return nil
}

// jobRefused is why l does not fit a Job Object whose memory limit holds at most memoryMax bytes,
// or nil. The process limit is a DWORD.
func jobRefused(l Limits, memoryMax uint64) error {
	switch {
	case l.TreeMemory > memoryMax:
		return errTreeMemoryWide
	case uint64(l.Processes) > math.MaxUint32: //nolint:gosec // G115: limitsRefused refused a negative Processes first
		return errProcessesWide
	}
	return nil
}

// seams are what a test of the package turns in one run. Only a _test.go file of the package sets
// Cmd.seam, which TestNoProgramThatImportsProcLinksTesting holds, so a program that imports proc has
// none and does not link testing.
type seams struct {
	// host stands in for this system's cgroups, on Linux: where the delegation check reads.
	host cgroupHost
	// procDir stands in for /proc, below which a File run's child execs its file, and which Run
	// checks is a proc mount.
	procDir string
}

// seams is c's, or none.
func (c Cmd) seams() seams {
	if c.seam == nil {
		return seams{}
	}
	return *c.seam
}
