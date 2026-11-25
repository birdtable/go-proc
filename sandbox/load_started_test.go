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
	dir := realTemp(t)
	report, outPath := filepath.Join(dir, "report"), filepath.Join(t.TempDir(), "out")
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	args := append([]string{self, loadChild, prog, report}, leave...)
	p, err := os.StartProcess(self, args, &os.ProcAttr{Env: []string{"PATH=/usr/bin:/bin"}, Files: []*os.File{null, out, out}}) //nolint:forbidigo // the row starts the test binary itself, as the process a caller sets the block on
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	why, _ := os.ReadFile(report)
	said, _ := os.ReadFile(outPath)
	return st.ExitCode(), string(why) + string(said)
}
