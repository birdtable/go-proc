//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// prGetNoNewPrivs is PR_GET_NO_NEW_PRIVS of linux/prctl.h.
const prGetNoNewPrivs = 39

// TestCheckOnThisLinuxKernelSaysYesAndSetsNothing: on a kernel that gives the
// block, as the one the tests run on must, Check with good folders and this
// test's own program, whose load folders it tells, is nil
// with Linux's Block, every field true, and the process has set nothing:
// no_new_privs, the first step the block
// takes, is still unset on this thread. Good folders hold the program, named by its real file;
// folders that leave its folder out are ErrUsage.
//
// What it costs when this fails: the ask starts the block on its caller's own process, or says no on a
// kernel the block runs on.
func TestCheckOnThisLinuxKernelSaysYesAndSetsNothing(t *testing.T) {
	t.Parallel()
	// no_new_privs is set only on the locked threads of the restrict tests,
	// which end with their goroutines, so no thread this test runs on has it.
	dir := realTemp(t, "rw", "ro")
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(Folders{ReadWrite: []string{dir + "/rw"}, ReadOnly: []string{dir + "/ro"}}, self); !errors.Is(err, ErrUsage) {
		t.Errorf("Check with folders that leave the program's out: %v, want ErrUsage", err)
	}
	b, err := Check(Folders{ReadWrite: []string{dir + "/rw"}, ReadOnly: []string{dir + "/ro", filepath.Dir(self)}}, self)
	if err != nil {
		t.Fatalf("Check with good folders: %v", err)
	}
	if b != linuxGives() {
		t.Errorf("Check with good folders: Block %+v, want Linux's, %+v", b, linuxGives())
	}
	if b, err = Check(Folders{}, ""); err != nil || b != linuxGives() {
		t.Fatalf("Check with no folders: Block %+v, %v, want Linux's, %+v", b, err, linuxGives())
	}
	set, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if set != 0 {
		t.Errorf("no_new_privs is set after Check, so the ask set a step of the block")
	}
}
