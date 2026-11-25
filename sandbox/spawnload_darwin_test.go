//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// spawnLoadWord, after the test binary's flags, makes TestAFailedSpawnUnderLoadReturnsItsError the
// row's child, which fails the spawns in a process of its own that its parent bounds.
const spawnLoadWord = "sandbox-spawnload"

// failedSpawns is how many spawns the row's child fails, shortSpawns how many under -short, and
// spawnBound how long its parent waits, sized by measure: with the spawn through syscall6, which gives up the P while runtime_BeforeExec holds the
// runtime's exec lock, the child at GOMAXPROCS=1 beside 4 spinning goroutines threw "stopm holding
// locks", then exited or hung in its own crash, in 5 of 20 runs of 50 spawns, 18 of 20 of 300, 19
// of 20 of 1000, 40 of 40 of 3000 and 40 of 40 of 10000. Through rawSyscall6 the 3000 take about
// 1 s on a Mac, and 1 to 3 s under -race at load 50, where 10000 took 3 to 10 s. The push tier's
// -short run fails 1000, under 1 s, to keep its budget, and is no proof.
const (
	failedSpawns, shortSpawns = 3000, 1000
	spawnBound                = 60 * time.Second
)

// TestAFailedSpawnUnderLoadReturnsItsError: on macOS, a child of the test, at GOMAXPROCS=1 with 4
// goroutines spinning, fails failedSpawns spawns (shortSpawns under -short) of a program that is
// not there, and each returns ENOENT; the parent waits spawnBound for it, since a spawn that gives
// up its P while the runtime's exec lock is held throws "stopm holding locks" and the child then
// may hang rather than exit. The block spawns through rawSyscall6, which keeps the P, as
// syscall.Exec's own execve does.
//
// What it costs when this fails: a caller with busy goroutines whose Run fails at the spawn, a
// program removed or not executable, crashes and hangs, holding cores, where Run should return
// ErrNoBlock with the spawn's error.
func TestAFailedSpawnUnderLoadReturnsItsError(t *testing.T) {
	if slices.Contains(os.Args, spawnLoadWord) {
		n := failedSpawns
		if testing.Short() {
			n = shortSpawns
		}
		failSpawnsUnderLoad(t, n)
		return
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "out")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	if v, ok := os.LookupEnv("GORACE"); ok {
		env = append(env, "GORACE="+v)
	}
	argv := []string{self, "-test.run=^TestAFailedSpawnUnderLoadReturnsItsError$", "-test.count=1", "-test.short=" + strconv.FormatBool(testing.Short()), spawnLoadWord}
	p, err := os.StartProcess(self, argv, &os.ProcAttr{Env: env, Files: []*os.File{nil, out, out}}) //nolint:forbidigo // the row starts the test binary itself, as its child
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	var hung atomic.Bool
	bound := time.AfterFunc(spawnBound, func() {
		hung.Store(true)
		_ = p.Kill()
	})
	st, err := p.Wait()
	bound.Stop()
	said, rerr := os.ReadFile(outPath)
	if err == nil {
		err = rerr
	}
	switch {
	case err != nil:
		t.Fatal(err)
	case hung.Load():
		t.Fatalf("the child did not end within %v of failing its spawns under a load: %s", spawnBound, tail(said))
	case !st.Success():
		t.Fatalf("the child failing its spawns under a load: %v: %s", st, tail(said))
	}
}

// failSpawnsUnderLoad is the row's child: at GOMAXPROCS=1, with 4 goroutines spinning, it fails n
// spawns of a program that is not there, and fails the test for each that gives anything but ENOENT.
func failSpawnsUnderLoad(t *testing.T, n int) {
	t.Helper()
	runtime.GOMAXPROCS(1)
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
			}
		})
	}
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()
	missing := filepath.Join(t.TempDir(), "no-program")
	enoent := func(string, []string, []string) error { return syscall.ENOENT }
	for i := range n {
		if err := spawnExec(enoent, missing, []string{missing}, nil); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("spawn %d of a program that is not there gave %v, want ENOENT", i, err)
		}
	}
}

// tail is the last 4 KiB of out, where a runtime's throw names its stack.
func tail(out []byte) []byte {
	if len(out) > 4096 {
		return out[len(out)-4096:]
	}
	return out
}
