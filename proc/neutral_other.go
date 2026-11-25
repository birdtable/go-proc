//go:build !windows

package proc

// NeutralDir is a folder that is no project's, for a Cmd's Dir: / here.
func NeutralDir() string { return "/" }
