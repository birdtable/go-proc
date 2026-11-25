//go:build linux

package proc

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the guard reads /proc below one descriptor by openat2 with RESOLVE_NO_XDEV, which syscall does not give
)

// treeIDHere is how this system names a tree: by its process group and its leader's start time.
const treeIDHere = idGroup

// listsBeforeStop is false: an exec here keeps the process's task and pid, so it stays in /proc's
// listing throughout, and frozenLeft lists the group once, frozen.
const listsBeforeStop = false

// leader reads the process group and start time of the process pid from <pid>/stat below r (stat):
// its fifth field and its twenty-second, in clock ticks since boot. A pid no process has is
// errNoProcess.
func leader(r procRoot, pid int) (pgid int, born uint64, err error) {
	fields, err := stat(r, pid)
	if err != nil {
		return 0, 0, err
	}
	pgid, errG := strconv.Atoi(fields[2])
	born, errB := strconv.ParseUint(fields[19], 10, 64)
	if err := errors.Join(errG, errB); err != nil {
		return 0, 0, fmt.Errorf("the stat of process %d: %w", pid, err)
	}
	return pgid, born, nil
}

// stat is the fields of <pid>/stat below r after the name, which may hold spaces and parentheses
// and ends at the last ')': state, ppid, pgrp (the third), ..., starttime (the twentieth), at least
// twenty of them. A pid no process has is errNoProcess.
func stat(r procRoot, pid int) ([]string, error) {
	b, err := r.read(r.fd, strconv.Itoa(pid)+"/stat")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil, errNoProcess
	}
	if err != nil {
		return nil, err
	}
	_, after, ok := strings.CutLast(string(b), ")")
	if !ok {
		return nil, fmt.Errorf("the stat of process %d has no name", pid)
	}
	fields := strings.Fields(after)
	if len(fields) < 20 {
		return nil, fmt.Errorf("the stat of process %d has %d fields after its name", pid, len(fields))
	}
	return fields, nil
}

// groupLeft is whether group pgid holds a process other than except, by a scan of /proc: any whose
// stat names the group, in any state, a zombie or one exiting among them. A process listed that ends
// before its stat is read is not one, and one born after the listing is not seen: the caller freezes
// the group first. A proc mount procOwn refuses, or a scan that fails, is true. A process this one is
// not shown, as one of another user under hidepid, is not seen. gap, in a test of the package, runs
// between the listing and the stats, and widens the time between them as a /proc of thousands of
// processes does.
func groupLeft(pgid, except int, gap func()) (left bool) {
	r, err := procOwn(procFS{dir: procMount})
	if err != nil {
		return true
	}
	defer func() { left = r.close() != nil || left }()
	fd, err := r.at(r.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY, below)
	if err != nil {
		return true
	}
	dir := os.NewFile(uintptr(fd), procMount)
	names, err := dir.Readdirnames(-1)
	if errors.Join(err, dir.Close()) != nil {
		return true
	}
	if gap != nil {
		gap()
	}
	group := strconv.Itoa(pgid)
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid == except {
			continue
		}
		fields, err := stat(r, pid)
		switch {
		case errors.Is(err, errNoProcess):
		case err != nil, fields[2] == group:
			return true
		}
	}
	return false
}

// errNoPlace is a proc mount that does not give the boot's id or this process's pid or time
// namespace, which a tree id here carries: one mounted subset=pid, as systemd's ProcSubset=pid gives
// a service, has no boot_id.
var errNoPlace = fmt.Errorf("proc: the proc mount does not give this boot's id and this process's pid and time namespaces, which a tree id here carries, so no detached tree is started or stopped by id: %w", errors.ErrUnsupported)

// place is where a tree id here is good, as the id carries it after the leader's pid and start
// time: this boot's id, from sys/kernel/random/boot_id below r, then the numbers of this process's
// pid namespace and time namespace, from self/ns/pid and self/ns/time, joined by dots. A pid and a
// start time in clock ticks name a process within one boot, one pid namespace and one time
// namespace alone: an id kept across a reboot, or handed to a sibling pid namespace, could name a
// new process that took the same pid at the same tick, and in another time namespace the start time
// reads otherwise. A kernel with no time namespaces has one, numbered 0 here.
//
// A container's sys is often a mount of proc's own sys, bound over itself to be read-only. So sys
// alone is crossed into where it is a mount, by no link, and only while its device is r's, which is
// one proc file system's; below it, as everywhere below r, no mount is crossed, and where openat2 is
// absent no file of another file system is read. So a file a runtime binds over
// sys/kernel/random/boot_id, as LXC (where lxc.autodev is on), Incus and systemd-nspawn do by their
// source, is no place, and Start and Stop there start and stop nothing.
func place(r procRoot) (string, error) {
	b, err := r.bootID()
	if err != nil {
		return "", fmt.Errorf("%w: reading this boot's id: %w", errNoPlace, err)
	}
	boot := strings.TrimSpace(string(b))
	if !bootForm(boot) {
		return "", fmt.Errorf("%w: this boot's id %q is not a UUID as the kernel writes one", errNoPlace, boot)
	}
	pidNS, err := r.namespace("self/ns/pid")
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoPlace, err)
	}
	timeNS, err := r.namespace("self/ns/time")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		timeNS = "0"
	case err != nil:
		return "", fmt.Errorf("%w: %w", errNoPlace, err)
	}
	return boot + "." + pidNS + "." + timeNS, nil
}

// bootID reads sys/kernel/random/boot_id below r, as place says.
func (r procRoot) bootID() (b []byte, err error) {
	fd, err := r.at(r.fd, "sys", unix.O_PATH|unix.O_DIRECTORY, unix.RESOLVE_BENEATH|unix.RESOLVE_NO_SYMLINKS)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unix.Close(fd)) }()
	if err := r.onRoot(fd); err != nil {
		return nil, fmt.Errorf("sys is a mount of another file system than the proc mount's: %w", err)
	}
	return r.read(fd, "kernel/random/boot_id")
}

// namespace is the number of the namespace the link rel below r names, as "pid:[4026531836]" names
// it. The link is opened itself, not followed, and read by its descriptor.
func (r procRoot) namespace(rel string) (id string, err error) {
	fd, err := r.at(r.fd, rel, unix.O_PATH, below)
	if err != nil {
		return "", fmt.Errorf("reading the namespace %s: %w", rel, err)
	}
	defer func() { err = errors.Join(err, unix.Close(fd)) }()
	buf := make([]byte, 64)
	n, err := unix.Readlinkat(fd, "", buf)
	if err != nil {
		return "", fmt.Errorf("reading the namespace %s: %w", rel, err)
	}
	l := string(buf[:n])
	_, num, _ := strings.Cut(l, ":[")
	num, ok := strings.CutSuffix(num, "]")
	if !ok || !decimal(num) || num == "0" {
		return "", fmt.Errorf("the namespace %s names %q, not a type and a number", rel, l)
	}
	return num, nil
}

// errProcMount is a proc mount that is not this pid namespace's own: missing, a folder or a link
// where the mount should be, or another namespace's, whose numbers name other processes than a
// kill here reaches.
var errProcMount = fmt.Errorf("proc: the proc mount is not this pid namespace's own, so the guard would read another process than the kill reaches, and nothing was started or signalled: %w", errors.ErrUnsupported)

// procRoot is the proc mount the guard reads, held open as fd from procOwn's check to the last read
// below it: what /proc names after the open, by a mount on it or a rename of its folder, is not
// read. dev is its device. flat is a kernel with no openat2, before Linux 5.6, or a filter that
// refuses it, where at opens by openat.
type procRoot struct {
	fd   int
	dev  uint64
	flat bool
}

// close closes r's descriptor.
func (r procRoot) close() error { return unix.Close(r.fd) }

// below is how at resolves a path below r: beneath the folder it starts from, through no magic link,
// and across no mount, so a file or folder bound below the proc mount is refused, not read.
const below = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_MAGICLINKS

// at opens rel below the folder dir, r's descriptor or one at opened, with flags, O_NOFOLLOW and
// O_CLOEXEC added, by openat2 resolving as resolve says. Where r is flat it opens by openat, which
// crosses a mount below dir, and refuses what it opened, wrapping EXDEV, unless it is on r's file
// system: a file or folder bound from another is refused so, but one of the same proc mount bound
// over another is read.
func (r procRoot) at(dir int, rel string, flags int, resolve uint64) (int, error) {
	flags |= unix.O_NOFOLLOW | unix.O_CLOEXEC
	var fd int
	var err error
	if r.flat {
		fd, err = unix.Openat(dir, rel, flags, 0)
		if err == nil {
			if err = r.onRoot(fd); err != nil {
				err = errors.Join(err, unix.Close(fd))
			}
		}
	} else {
		fd, err = unix.Openat2(dir, rel, &unix.OpenHow{Flags: uint64(flags), Resolve: resolve}) //nolint:gosec // G115: open's flags are not negative
	}
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: rel, Err: err}
	}
	return fd, nil
}

// onRoot is EXDEV unless fd is on r's file system, by its device. Dev is 32 bits on mips.
func (r procRoot) onRoot(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if uint64(st.Dev) != r.dev {
		return unix.EXDEV
	}
	return nil
}

// read is the contents of rel below the folder dir, as at opens it.
func (r procRoot) read(dir int, rel string) ([]byte, error) {
	fd, err := r.at(dir, rel, unix.O_RDONLY, below)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), rel)
	b, err := io.ReadAll(f)
	return b, errors.Join(err, f.Close())
}

// procOwn opens and checks p's dir, the mount the guard reads, as the kernel's and this pid
// namespace's own, before Start starts or Stop signals anything, and holds it open for every read
// after (procRoot). First it is a folder, not a link, and the root of a proc file system: its type
// is proc's and its inode is 1, so neither a folder, in a root whose /proc is one, where a planted
// stat would be read as a reaped leader's and its group killed, nor a folder of proc bound on /proc
// passes. Then its self/stat opens, and its first field is this process's pid; and its self/status
// opens, and its NSpid line holds one number, this process's pid (nspidOwn). A mount of an outer pid
// namespace, whose self/stat may name a pid equal to this one's by chance, lists two numbers or
// more there.
//
// Each read below it goes by openat2 with RESOLVE_NO_XDEV, so a file or folder bound below the
// mount is refused. Two routes stay open, and are not guarded against:
//   - a kernel with no openat2, before Linux 5.6, or a filter that refuses it with EPERM, where each
//     read goes by openat, which crosses a mount below. Each file and folder it opens must be on the
//     mount's own file system, so one bound from another is refused, wrapping EXDEV; but a file or
//     folder of the same proc mount bound over another, as one process's stat or folder over a
//     leader's, is read there as the kernel's;
//   - a file system whose server supplies the type fstatfs gives, as 9p's does: a folder on one whose
//     server answers proc's type and inode 1 passes as a proc mount.
func procOwn(p procFS) (procRoot, error) {
	fd, err := unix.Open(p.dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return procRoot{}, fmt.Errorf("%w: %s is missing, or is a link or no folder: %w", errProcMount, p.dir, err)
	}
	r := procRoot{fd: fd, flat: p.noOpenat2}
	if err := r.own(p.standIn); err != nil {
		return procRoot{}, errors.Join(fmt.Errorf("%w: %s: %w", errProcMount, p.dir, err), r.close())
	}
	return r, nil
}

// own is procOwn's checks of r, its type and inode taken as given where typeTaken, for a test's
// stand-in; it sets r's device, and r flat where openat2 is absent or refused.
func (r *procRoot) own(typeTaken bool) error {
	var st unix.Stat_t
	var sf unix.Statfs_t
	if err := errors.Join(unix.Fstat(r.fd, &st), unix.Fstatfs(r.fd, &sf)); err != nil {
		return err
	}
	if !typeTaken && (sf.Type != procSuperMagic || st.Ino != 1) {
		return errors.New("not the root of a proc file system, so the files below it need not be the kernel's")
	}
	r.dev = uint64(st.Dev)
	b, err := r.read(r.fd, "self/stat")
	if !r.flat && (errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM)) {
		r.flat = true
		b, err = r.read(r.fd, "self/stat")
	}
	if err != nil {
		return err
	}
	if first, _, _ := strings.Cut(string(b), " "); first != strconv.Itoa(os.Getpid()) {
		return fmt.Errorf("self/stat names process %q, not this one, %d", first, os.Getpid())
	}
	s, err := r.read(r.fd, "self/status")
	if err != nil {
		return err
	}
	if why := nspidOwn(string(s), os.Getpid()); why != "" {
		return fmt.Errorf("self/status: %s", why)
	}
	return nil
}

// groupMembers lists nothing here: Linux's one kill reaches every member (killAgain is false), so
// stopTree never goes on by member.
func groupMembers(int) ([]member, error) {
	return nil, fmt.Errorf("proc: a group is not listed by member on Linux: %w", errors.ErrUnsupported)
}
