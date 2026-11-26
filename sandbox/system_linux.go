//go:build linux

package sandbox

// runHere is Run on Linux: the block set on this process, with the files
// held to n, then the exec.
func runHere(n named, prog string, argv, environ []string) error {
	return runLinux(linuxKernel{}, builtArch, n, prog, argv, environ)
}

// checkHere is Check on Linux: the filter for this architecture, Landlock and,
// with folders, what RunHolding needs for them, asked of this kernel, with
// nothing set.
func checkHere(n named) error { return checkLinux(linuxKernel{}, builtArch, n) }

// blockHere is the Block Check says Linux gives: its row of blockOn.
func blockHere() Block { return blockOn("linux") }

// programHere is how the Linux block holds the program: its path walked, with the 40 links of
// MAXSYMLINKS, and its real file beneath a folder, as Landlock holds it at the exec. A link on the
// path is not held, as Landlock has no right for a lookup.
func programHere() programRule { return programRule{walks: true, maxLinks: 40} }
