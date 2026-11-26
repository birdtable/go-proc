//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// copier is this system's cmd, by its real path, as the live row's program.
func copier(t *testing.T) string {
	t.Helper()
	prog, err := filepath.EvalSymlinks(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"))
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// copyArgs are cmd's arguments that copy src to dst, with no question if dst is there.
func copyArgs(src, dst string) []string { return []string{"/c", "copy", "/y", src, dst} }
