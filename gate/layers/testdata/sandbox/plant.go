// Package sandbox is a planted sandbox: its one import of the module that the row does not allow is red.
package sandbox

import (
	_ "github.com/birdtable/go-proc/load" // want layers-sandbox
	_ "github.com/birdtable/go-proc/proc"
)
