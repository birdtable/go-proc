//go:build !((linux && (amd64 || arm64)) || darwin)

package main

// reexec has nothing to run on a system the kernel proof is not built for: the test binary is never
// the command or the row child there.
func reexec([]string) (int, bool) { return 0, false }
