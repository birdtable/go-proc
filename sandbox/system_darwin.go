//go:build darwin

package sandbox

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// runHere is Run on macOS: the streams checked, then the system sandbox,
// with the files held to n.
func runHere(n named, prog string, argv, environ []string) error {
	return runMac(darwinKernel{}, n, prog, argv, environ)
}

// checkHere is Check on macOS: the system sandbox's program is there to run.
func checkHere(named) error { return checkMac(os.Stat) }

// blockHere is the Block Check says macOS gives: its row of blockOn.
func blockHere() Block { return blockOn("darwin") }

// loadHere is the folders prog needs to load on macOS, beyond its own: none, by measure. On macOS
// 27.2 /usr/bin/true, linked against libSystem, starts and exits 0 behind RunHolding with /usr/bin
// alone: the system's libraries are in dyld's shared cache, which the kernel maps
// into the program and no file read opens, and the profile gives back the read of the root
// folder's own list, which the system sandbox needs to start a program. A library outside the
// shared cache, in a folder that is not the program's, is the caller's to name.
func loadHere(string, string, []string) ([]string, error) { return nil, nil }

// programHere is how the macOS block holds the program: its path walked, with the 32 links of
// MAXSYMLINKS, its real file beneath a folder, every link on the path beneath one too, as the
// system sandbox refuses to follow any other, and a #! script's interpreter held the same way, as
// the system sandbox holds that exec too.
func programHere() programRule {
	return programRule{walks: true, maxLinks: 32, linksHeld: true, interpreters: true}
}

// mayRunHere is nil when this caller may run the file, as the macOS kernel asks it before any
// interpreter reads the file, and otherwise why not, wrapping EACCES or the error it was given.
// The kernel asks two things. The file's mode must have an execute bit, whoever it is for: a file
// with none it refuses whatever an ACL allows. And faccessat(2) with X_OK and AT_EACCESS, by the
// effective ids the exec runs with, must say yes: it reads the caller's own bit, never the three
// together, and the ACL.
func mayRunHere(file string) error {
	info, err := os.Stat(file)
	switch {
	case err != nil:
		return err
	case info.Mode().Perm()&0o111 == 0:
		return fmt.Errorf("its mode %v has no execute bit, and the kernel runs no such file whatever its ACL allows: %w", info.Mode().Perm(), unix.EACCES)
	}
	if err := unix.Faccessat(unix.AT_FDCWD, file, unix.X_OK, unix.AT_EACCESS); err != nil {
		return fmt.Errorf("faccessat(2) with X_OK, by the effective ids the exec runs with, refuses it: %w", err)
	}
	return nil
}
