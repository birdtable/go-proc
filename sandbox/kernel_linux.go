//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"github.com/landlock-lsm/go-landlock/landlock"
	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
	"golang.org/x/sys/unix"
)

// linuxKernel is the kernel the Linux block asks, through package syscall,
// and through go-landlock for the Landlock ruleset.
type linuxKernel struct{}

// noNewPrivs sets no_new_privs on the thread runLinux locks: x/sys's Prctl
// is a Syscall, not a RawSyscall, and the locked goroutine stays on it.
func (linuxKernel) noNewPrivs() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

// closeAtExec marks fd and every descriptor above it to close at exec
// (CLOSE_RANGE_CLOEXEC), so the Go runtime keeps its own until then.
func (linuxKernel) closeAtExec(fd int) error {
	return unix.CloseRange(uint(fd), math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)
}

func (linuxKernel) stdioFamilies() ([3]int, error) {
	return socketFamilies(func(fd int) (int, error) {
		return syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_DOMAIN)
	})
}

// landlockABI asks go-landlock's call for the ABI the kernel speaks, and
// for the errata it has fixed. A kernel that answers no errata, which one
// older than the errata has, has fixed none.
func (linuxKernel) landlockABI() (int, int, error) {
	abi, err := ll.LandlockGetABIVersion()
	if err != nil {
		return 0, 0, err
	}
	errata, err := ll.LandlockGetErrata()
	if err != nil {
		errata = 0
	}
	return abi, errata, nil
}

// procFDs stats /proc/self/fd, by which restrictHeld names each rule's
// folder to go-landlock.
func (linuxKernel) procFDs() error {
	_, err := os.Stat("/proc/self/fd")
	return err
}

// tempFolder is the temporary folder by its real path, as holdOutside makes
// the probe's folder under it.
func (linuxKernel) tempFolder() (string, error) {
	return filepath.EvalSymlinks(os.TempDir())
}

// errNoRuleset is go-landlock making no ruleset for a block that asks for
// one: a descriptor below zero, which it gives when there is nothing to
// enforce.
var errNoRuleset = errors.New("go-landlock made no ruleset, so nothing would restrict the program")

// restrict makes the ruleset through go-landlock's Config.NewRulesetFD, with
// each rule's folder opened here first, and restricts this thread to it.
// go-landlock opens the paths it is given and follows a symbolic link, so a
// folder is opened by O_PATH with no link followed in any name of its path,
// and named to go-landlock by its descriptor (restrictHeld): the rule then
// grants the folder that was opened, even if a link took its place, or the
// place of a folder above it, since it was checked. Each descriptor is
// closed once the ruleset is made.
func (linuxKernel) restrict(handledFS, scoped uint64, rules []pathRule) (err error) {
	held, err := openRules(rules)
	defer func() {
		for _, r := range held {
			err = errors.Join(err, syscall.Close(r.fd))
		}
	}()
	if err != nil {
		return err
	}
	return restrictHeld(handledFS, scoped, held)
}

// heldRule is a rule whose folder is open, by its descriptor.
type heldRule struct {
	pathRule
	fd int
}

// openRules opens each rule's folder by O_PATH, following no link in any name
// of its path, and returns those it opened, which the caller closes, with the
// first error. Each folder is named by its real path, so a link anywhere in
// it was put there since it was checked: a link in a folder above is refused
// with ELOOP, and the folder itself, if it is a link, with ENOTDIR.
func openRules(rules []pathRule) ([]heldRule, error) {
	held := make([]heldRule, 0, len(rules))
	for _, r := range rules {
		dir, err := unix.Openat2(unix.AT_FDCWD, r.dir, &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			return held, &os.PathError{Op: "open", Path: r.dir, Err: err}
		}
		held = append(held, heldRule{r, dir})
	}
	return held, nil
}

// restrictHeld makes the ruleset from rules whose folders are open, each
// named to go-landlock by its descriptor's /proc/self/fd path and never by
// its own, and restricts this thread to it.
func restrictHeld(handledFS, scoped uint64, rules []heldRule) error {
	cfg := landlock.Config{HandledAccessFS: landlock.AccessFSSet(handledFS), Scoped: landlock.ScopedSet(scoped)}
	lrules := make([]landlock.Rule, 0, len(rules))
	for _, r := range rules {
		lrules = append(lrules, landlock.PathAccess(landlock.AccessFSSet(r.allowed), "/proc/self/fd/"+strconv.Itoa(r.fd)))
	}
	fd, flags, err := cfg.NewRulesetFD(lrules...)
	if err != nil {
		return fmt.Errorf("go-landlock could not make the ruleset: %w", err)
	}
	if fd < 0 {
		return errNoRuleset
	}
	err = ll.LandlockRestrictSelf(fd, flags)
	return errors.Join(err, syscall.Close(fd))
}

// holdOutside binds the probe's two Unix sockets in a folder made for them
// under the temporary folder, by its real path, each closed at exec, so the
// program does not inherit them, and writes its file there. Each socket and
// the file are made their owner's to read and write whatever the umask,
// since a connect needs write on the socket: a refusal is then not the
// host's. The stream listens with room for the control's connect and the
// probe's.
func (linuxKernel) holdOutside() (held, error) {
	made, err := os.MkdirTemp("", "netblock-")
	if err != nil {
		return held{}, err
	}
	dir, err := filepath.EvalSymlinks(made)
	if err != nil {
		return held{}, errors.Join(err, os.RemoveAll(made))
	}
	h := held{stream: -1, dgram: -1, dir: dir, streamPath: filepath.Join(dir, "s"), dgramPath: filepath.Join(dir, "d"), filePath: filepath.Join(dir, "f")}
	if h.stream, err = boundAt(syscall.SOCK_STREAM, h.streamPath); err == nil {
		err = syscall.Listen(h.stream, 2)
	}
	if err == nil {
		h.dgram, err = boundAt(syscall.SOCK_DGRAM, h.dgramPath)
	}
	if err == nil {
		err = os.WriteFile(h.filePath, []byte{0}, 0o600)
	}
	if err == nil {
		err = os.Chmod(h.filePath, 0o600)
	}
	if err != nil {
		return held{}, errors.Join(err, releaseHeld(h))
	}
	return h, nil
}

// boundAt is a Unix socket of typ, closed at exec, bound at path and made
// its owner's to read and write.
func boundAt(typ int, path string) (int, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, typ|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return -1, errors.Join(err, syscall.Close(fd))
	}
	if err := syscall.Chmod(path, 0o600); err != nil {
		return -1, errors.Join(err, syscall.Close(fd))
	}
	return fd, nil
}

// reach connects a new stream socket to h's stream, sends one datagram from
// a new socket to h's datagram socket by sendto, closing both, makes an
// AF_INET stream socket and closes it, connecting nowhere, and opens h's
// file to read and to write, closing each. Landlock judges a file when it is
// opened.
func (linuxKernel) reach(h held) reached {
	var r reached
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err == nil {
		err = errors.Join(syscall.Connect(fd, &syscall.SockaddrUnix{Name: h.streamPath}), syscall.Close(fd))
	}
	r.stream = err
	fd, err = syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err == nil {
		err = errors.Join(syscall.Sendto(fd, []byte{0}, 0, &syscall.SockaddrUnix{Name: h.dgramPath}), syscall.Close(fd))
	}
	r.dgram = err
	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err == nil {
		err = syscall.Close(fd)
	}
	r.inet = err
	r.read = opened(h.filePath, syscall.O_RDONLY)
	r.write = opened(h.filePath, syscall.O_WRONLY)
	return r
}

// opened opens path with mode, closed at exec, and closes it.
func opened(path string, mode int) error {
	fd, err := syscall.Open(path, mode|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}

// release closes h's sockets, those made, and removes its folder. The
// ruleset that holds the files denies this thread the removal, so it is done
// by a goroutine, which runs on another thread: the runtime runs no other
// goroutine on a thread one has locked, and makes each new thread from a
// thread of its own, made when this one was locked, before the ruleset.
func (linuxKernel) release(h held) error {
	done := make(chan error, 1)
	go func() { done <- releaseHeld(h) }()
	return <-done
}

// releaseHeld is release, on the thread that calls it.
func releaseHeld(h held) error {
	var errs []error
	for _, fd := range []int{h.stream, h.dgram} {
		if fd >= 0 {
			errs = append(errs, syscall.Close(fd))
		}
	}
	return errors.Join(append(errs, os.RemoveAll(h.dir))...)
}

// loadFilter sets prog on every thread. The kernel takes the program's
// length in 16 bits, and its instructions as syscall lays them out.
func (linuxKernel) loadFilter(prog []sockFilter) error {
	n := len(prog)
	if n == 0 || n > math.MaxUint16 {
		return syscall.EINVAL
	}
	a, ok := filterArches()[builtArch]
	if !ok {
		return syscall.EINVAL
	}
	filter := make([]syscall.SockFilter, n)
	for i, in := range prog {
		filter[i] = syscall.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	fprog := syscall.SockFprog{Len: uint16(n), Filter: &filter[0]}
	r1, _, errno := syscall.RawSyscall(uintptr(a.seccomp), seccompSetModeFilter, seccompFilterFlags, uintptr(unsafe.Pointer(&fprog))) //nolint:gosec // G103: the call takes its program by address, and syscall has no wrapper
	return filterLoaded(r1, errno)
}

func (linuxKernel) exec(prog string, argv, env []string) error {
	return syscall.Exec(prog, argv, env) //nolint:forbidigo,gosec // G204: the block's own exec, of the program by the absolute path its caller gave, behind what was just set
}
