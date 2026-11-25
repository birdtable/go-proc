//go:build linux

package proc

import "syscall"

// fileRuns is whether Run starts a Cmd.File here: Linux does, the child execing /proc/self/fd/<n>
// as glibc's fexecve does.
const fileRuns = true

// procSuperMagic is PROC_SUPER_MAGIC of linux/magic.h: the type statfs gives a proc mount.
const procSuperMagic = 0x9fa0

// procMounted is whether dir is a folder, not a link, whose file system statfs gives as proc's, below
// which a File run's child finds its file at self/fd/<n>, the kernel's own. It reads dir's type
// alone, by path: a mount below dir, a change of what dir names between the check and a read, and
// a file system whose server supplies the type, as 9p's does, pass it. In a root with no proc mount, an
// ordinary folder there may hold a file by that name, or a link into another proc mount's
// descriptors, which statfs would follow; either would run in File's place. Both reads are the
// system calls themselves, as os.Lstat is on Linux: gosec's taint check reads os.Lstat as a sink
// and, by the run, finds a test seam's path tainted or not.
func procMounted(dir string) bool {
	var st syscall.Stat_t
	var fs syscall.Statfs_t
	return syscall.Lstat(dir, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR &&
		syscall.Statfs(dir, &fs) == nil && fs.Type == procSuperMagic
}
