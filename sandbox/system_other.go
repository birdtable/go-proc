//go:build !linux && !darwin

package sandbox

// runHere is Run on a system with no block: it refuses, and runs nothing.
func runHere(named, string, []string, []string) error { return runNone() }

// checkHere is Check on a system with no block: no.
func checkHere(named) error { return runNone() }

// blockHere is the Block of a system with no block: nothing closed. Check
// never returns it with a yes, as checkHere refuses first.
func blockHere() Block { return Block{} }

// loadHere is the folders a program needs to load on a system with no block: none, and the
// program is not read, as runHere and checkHere refuse.
func loadHere(string, string, []string) ([]string, error) { return nil, nil }

// programHere is how a system with no block holds the program: not at all, and its path is not
// walked, as runHere and checkHere refuse.
func programHere() programRule { return programRule{} }

// mayRunHere is whether a file may run on a system with no block: it is never asked, as no
// program rule holds an interpreter here, and it says so.
func mayRunHere(string) error { return runNone() }
