//go:build !linux

package proc

import (
	"os"
	"testing"
)

// peakHere is whether a run here reads its peak: this system's peakReads.
func peakHere(*testing.T) bool { return peakReads }

// nulNamedFile is Linux's alone, as only Linux runs a File (fileRuns).
func nulNamedFile(t *testing.T) *os.File {
	t.Helper()
	t.Fatal("no File runs here")
	return nil
}
