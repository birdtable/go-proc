//go:build darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// heldStarts is how many times the row starts the command, sized by measure: with
// no lock held from the marking to the exec, as few as 22 of 300 programs held a pipe on a Mac
// under -race, so 100 starts all miss it with odds of about 1 in 2000. The push tier's -short run
// starts it 10 times, about 0.3 s, to keep its budget, and is no proof; the release tier and every
// native run start it 100 times, about 2.5 s under -race.
const heldStarts, shortStarts = 100, 10

// unmarkedStarts is how many times a row starts the command under a load whose descriptors nothing
// marks in the call that makes them, sized by measure: on v0.12.0, which execs by
// syscall.Exec under syscall.ForkLock, 150 of 240 programs held a descriptor received in an
// SCM_RIGHTS message and 131 of 240 one from a raw dup, on a Mac under -race with the four loaded
// rows side by side, so 20 starts all miss either with odds under 1 in a million, and the -short
// run's 10 with odds under 1 in 2000. An accepted socket is rarer: 76 of 900 programs held one by
// the same measure, as few as 4 of a row's 100, so its row starts the command heldStarts times,
// which all miss it with odds of about 1 in 6000, and its -short run is no proof.
const unmarkedStarts = 20

// TestAPipeAnotherGoroutineMakesAsTheBlockIsSetDoesNotReachTheProgram: on macOS, the command is
// started heldStarts times (shortStarts under -short) with 4 goroutines making and closing pipes as it sets the block, and in
// each the program, the row child, finds no descriptor open above stderr before it opens one.
// macOS makes a pipe and marks it to close at exec in two calls, under syscall.ForkLock held for
// reading, so the block holds that lock from its last marking into the exec.
//
// What it costs when this fails: a pipe or a socket another goroutine of the caller makes as Run
// execs reaches the program open, and a connected socket reaches the network behind the block.
func TestAPipeAnotherGoroutineMakesAsTheBlockIsSetDoesNotReachTheProgram(t *testing.T) {
	t.Parallel()
	noneHeldUnder(t, "pipes", nil, heldStarts)
}

// noneHeldUnder starts the command starts times (shortStarts under -short) under the load named,
// with extra from descriptor 3, and fails the test for each start whose program, the row child,
// finds a descriptor open above stderr before it opens one, or does not run.
func noneHeldUnder(t *testing.T, load string, extra []*os.File, starts int) {
	t.Helper()
	if testing.Short() {
		starts = shortStarts
	}
	p := newProof(t)
	dir := t.TempDir()
	held := 0
	for i := range starts {
		report := filepath.Join(dir, strconv.Itoa(i))
		code, out := p.start(t, nil, extra, loadedWord, load, "--", p.self, rowWord, report, "descriptors")
		said, err := os.ReadFile(report)
		if code != 0 || err != nil {
			t.Fatalf("start %d: the command did not run the program: exit %d, %v: %s", i, code, err, out)
		}
		if string(said) != "none" {
			held++
			t.Errorf("start %d: the program held, above stderr: %s", i, said)
		}
	}
	if held > 0 {
		t.Errorf("of %d starts under a load of %s, %d programs held a descriptor", starts, load, held)
	}
}

// dialed is 4 Unix socket listeners of the test's own, as the files a command takes at descriptors
// 3 to 6, each with a goroutine of the test dialing it and closing each connection until the test
// ends: the connections a command's accepts load accepts, made outside the command, so its own
// goroutines make no socket but by accepting.
func dialed(t *testing.T) []*os.File {
	t.Helper()
	dir := shortDir(t)
	var stop atomic.Bool
	var wg sync.WaitGroup
	files := make([]*os.File, 4)
	for i := range files {
		path := filepath.Join(dir, strconv.Itoa(i))
		l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
		f, err := l.(*net.UnixListener).File()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		files[i] = f
		wg.Go(func() {
			for !stop.Load() {
				if c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path); err == nil {
					_ = c.Close()
				}
			}
		})
	}
	t.Cleanup(func() {
		stop.Store(true)
		wg.Wait()
	})
	return files
}

// TestASocketAnotherGoroutineAcceptsAsTheBlockIsSetDoesNotReachTheProgram: on macOS, the command
// is started heldStarts times (shortStarts under -short) with 4 goroutines each accepting on a Unix
// socket listener of its own, which the test dials from outside, and closing each connection as it sets the
// block, and in each the program, the row child, finds no descriptor open above stderr before it
// opens one. On macOS Go accepts a socket and marks it to close at exec in two calls, under no
// lock, and syscall.ForkLock held by the block does not hold it off: the block
// starts the program by posix_spawn with POSIX_SPAWN_CLOEXEC_DEFAULT, where the kernel closes every
// descriptor but 0 to 2 whatever its mark. With syscall.Exec under the lock, as
// v0.12.0 had it, 76 of 900 programs held an accepted socket.
//
// What it costs when this fails: a connection a server's goroutine accepts as Run execs reaches
// the program open, and the program talks over it to a peer on the network behind the block.
func TestASocketAnotherGoroutineAcceptsAsTheBlockIsSetDoesNotReachTheProgram(t *testing.T) {
	t.Parallel()
	noneHeldUnder(t, "accepts", dialed(t), heldStarts)
}

// TestADescriptorAnotherGoroutineReceivesAsTheBlockIsSetDoesNotReachTheProgram: on macOS, the
// command is started unmarkedStarts times (shortStarts under -short) with 4 goroutines receiving a
// descriptor in an SCM_RIGHTS message by ReadMsgUnix, and closing it, as it sets the block, and in
// each the program, the row child, finds no descriptor open above stderr before it opens one.
// On macOS Go receives the descriptor unmarked and marks it in a later call, under no lock, and
// only the kernel closing every descriptor but 0 to 2 at the spawn keeps it from the program. With syscall.Exec, as v0.12.0 had it, 150 of 240 programs held one.
//
// What it costs when this fails: a descriptor a peer passes the caller as Run execs, a connected
// socket or a file outside the folders, reaches the program open, past the block.
func TestADescriptorAnotherGoroutineReceivesAsTheBlockIsSetDoesNotReachTheProgram(t *testing.T) {
	t.Parallel()
	noneHeldUnder(t, "rights", nil, unmarkedStarts)
}

// TestADescriptorAnotherGoroutineDupsAsTheBlockIsSetDoesNotReachTheProgram: on macOS, the command
// is started unmarkedStarts times (shortStarts under -short) with 4 goroutines that dup stdin by a
// raw call, which marks nothing, and close it, as it sets the block, and in each the program, the
// row child, finds no descriptor open above stderr before it opens one: the kernel closes every
// descriptor but 0 to 2 at the spawn, whatever its mark. With syscall.Exec, as
// v0.12.0 had it, 131 of 240 programs held one. On Linux the block execs by execve, and a raw dup
// stays the exception the package's doc names.
//
// What it costs when this fails: a descriptor a goroutine of the caller dups as Run execs, of a
// connected socket or a file outside the folders, reaches the program open, past the block.
func TestADescriptorAnotherGoroutineDupsAsTheBlockIsSetDoesNotReachTheProgram(t *testing.T) {
	t.Parallel()
	noneHeldUnder(t, "dups", nil, unmarkedStarts)
}
