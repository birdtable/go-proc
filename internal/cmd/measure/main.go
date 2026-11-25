// Command measure checks NOTICE against go.mod and prints the size of the module's code. The
// Taskfile runs it from the module's root, and it starts no program:
//
//	measure notice-check  fails when NOTICE does not name each module of the build at its version,
//	                      read from go list's lines on its standard input, or names one go.mod lacks
//	measure size          prints the non-test Go lines and fails above the ceiling
//
// The exported surface is held by apidiff in task api, not here.
package main

import "os"

func main() { os.Exit(run(os.Args[1:], ".", os.Stdin, os.Stdout)) }
