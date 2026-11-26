//go:build linux || darwin

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// behindTheBlock starts the test binary as LoadChild with the program prog, and leave when given,
// and returns the program's exit, or LoadChild's with why it did not run, and what it printed.
func behindTheBlock(t *testing.T, prog string, leave ...string) (int, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(realTemp(t), "report")
	code, said := started(t, []string{"PATH=/usr/bin:/bin"}, append([]string{self, loadChild, prog, report}, leave...))
	why, _ := os.ReadFile(report)
	return code, string(why) + said
}
