//go:build darwin

package sandbox

import (
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
func loadHere(string, string) ([]string, error) { return nil, nil }

// programHere is how the macOS block holds the program: its path walked, with the 32 links of
// MAXSYMLINKS, its real file beneath a folder, every link on the path beneath one too, as the
// system sandbox refuses to follow any other, and a #! script's interpreter held the same way, as
// the system sandbox holds that exec too.
func programHere() programRule {
	return programRule{walks: true, maxLinks: 32, linksHeld: true, interpreters: true}
}

// mayRunHere is nil when this caller may run the file, as access(2) with X_OK asks the macOS
// kernel, which refuses the exec with EACCES before any interpreter reads the file: the program
// runs as the caller, whose own execute bit decides, or its group's, or the others', never the
// three bits together.
func mayRunHere(file string) error { return unix.Access(file, unix.X_OK) }
