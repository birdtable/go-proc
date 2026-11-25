//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// darwinKernel is the kernel the macOS block asks, through package syscall.
// bound is 0 but in a test of the package, which shortens the dry run's
// timeout to run its end by the bound; dryRun takes it only under
// testing.Testing().
type darwinKernel struct {
	bound time.Duration
}

// closeAtExec marks each descriptor /dev/fd names from fd up to close at exec, read by its names alone: os.ReadDir stats each
// and leaves out one whose stat says EBADF, as it says now and then of one held open while other goroutines look up /dev/fd.
// The listing's own descriptor is closed by the time it is marked, and marking a closed one does nothing.
func (darwinKernel) closeAtExec(fd int) error {
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	switch err = errors.Join(err, dir.Close()); {
	case err != nil:
		return err
	case len(names) < 3:
		return errors.New("/dev/fd lists fewer than the three standard streams, so the descriptors open cannot be told")
	}
	for _, name := range names {
		n, err := strconv.Atoi(name)
		if err != nil {
			return errors.New("/dev/fd lists " + strconv.Quote(name) + ", which is not a descriptor")
		}
		if n >= fd {
			syscall.CloseOnExec(n)
		}
	}
	return nil
}

func (darwinKernel) stdioFamilies() ([3]int, error) {
	return socketFamilies(func(fd int) (int, error) {
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			return 0, err
		}
		switch sa.(type) {
		case *syscall.SockaddrUnix:
			return syscall.AF_UNIX, nil
		case *syscall.SockaddrInet4:
			return syscall.AF_INET, nil
		case *syscall.SockaddrInet6:
			return syscall.AF_INET6, nil
		}
		return -1, nil
	})
}

func (darwinKernel) exec(prog string, argv, env []string) error {
	return spawnExec(syscall.Exec, prog, argv, env) //nolint:forbidigo // the block's own exec: syscall.Exec only puts the open-file limit back, for no path, and posix_spawn execs
}

// Bounds of the dry run: sandbox-exec sets a profile and runs macNoop, and
// what it writes is its reason for a profile it refused.
const (
	dryRunTimeout = 10 * time.Second
	dryRunOutput  = 4096
)

// dryRun runs path through proc, the runner, in a tree of its own that ends
// with it, with no environment, so nothing of this process's reaches the
// sandbox, bounded by dryRunTimeout, its Drain too, and dryRunOutput. A dry run that ran and
// was ended, by its bound, its output cap or a signal, is code -1 with what it
// wrote, not an error: proc gives each as one, and the error is kept for a
// dry run that could not be run.
func (k darwinKernel) dryRun(path string, args []string) (int, []byte, error) {
	timeout := dryRunTimeout
	if testing.Testing() && k.bound > 0 {
		timeout = k.bound
	}
	r, err := proc.Run(context.Background(), proc.Cmd{Path: path, Args: args, Env: []string{}, Timeout: timeout, MaxOutput: dryRunOutput, Drain: timeout}) //nolint:forbidigo // go.concurrency.1: Run takes no context, since its exec ends this program, and the dry run is bounded by its own Timeout
	if ended(err) {
		return -1, r.Stderr, nil
	}
	return r.Code, r.Stderr, err
}

// signalled is how proc gives a child a signal ended: os/exec's exit error,
// whose ExitCode is -1. It is matched by its method, since only proc may
// import os/exec.
type signalled interface {
	error
	ExitCode() int
}

// ended is whether err is a run that ran and was ended: by its bound or its
// output cap (proc.Ended), or by a signal.
func ended(err error) bool {
	if _, ok := errors.AsType[proc.Ended](err); ok {
		return true
	}
	s, ok := errors.AsType[signalled](err)
	return ok && s.ExitCode() < 0
}
