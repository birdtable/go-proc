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
