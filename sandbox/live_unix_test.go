//go:build unix

package sandbox

import (
	"path/filepath"
	"testing"
)

// copier is this system's cp, by the file its links resolve to, as the live row's program.
func copier(t *testing.T) string {
	t.Helper()
	prog, err := filepath.EvalSymlinks("/bin/cp")
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// copyArgs are cp's arguments that copy src to dst.
func copyArgs(src, dst string) []string { return []string{src, dst} }
