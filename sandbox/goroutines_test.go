package sandbox

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/birdtable/go-proc/load"
	"github.com/birdtable/go-testkit"
)

// self is the prefix of every function of this package as a stack names it. The dot after the path
// keeps out a package beneath this one's folder, such as sandbox/cmd/netblock.
const self = "github.com/birdtable/go-proc/sandbox."

// goroutine is one goroutine of the process as runtime.Stack lists it: its id and its stack.
type goroutine struct {
	id, stack string
}

// goroutines is every goroutine of the process but the runtime's own, read by runtime.Stack.
func goroutines() []goroutine {
	buf := make([]byte, 1<<16)
	for {
		if n := runtime.Stack(buf, true); n < len(buf) {
			return parseGoroutines(string(buf[:n]))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// parseGoroutines reads runtime.Stack's listing: blocks parted by a blank line, each headed
// "goroutine <id> [<state>]:".
func parseGoroutines(s string) []goroutine {
	var out []goroutine
	for block := range strings.SplitSeq(s, "\n\n") {
		head, stack, _ := strings.Cut(block, "\n")
		rest, ok := strings.CutPrefix(head, "goroutine ")
		id, _, spaced := strings.Cut(rest, " ")
		if ok && spaced && id != "" {
			out = append(out, goroutine{id, stack})
		}
	}
	return out
}

// runsHere says whether g has a frame of this package's code. A frame is a line of a function's name;
// a file's line starts with a tab, and the "created by" line names the code that started g, not
// code g runs.
func (g goroutine) runsHere() bool {
	for line := range strings.SplitSeq(g.stack, "\n") {
		if strings.HasPrefix(line, self) {
			return true
		}
	}
	return false
}

// leftOver is each goroutine that runs this package's code and is not one of before's, by id, once
// none is left or wait has passed, whichever comes first, yielding between reads: a goroutine has no
// signal for its end, so the fact is read again until it holds. A goroutine of another package's code alone
// is not counted, whoever started it: the runtime's, a timer's or a pipe's are no leak of this one.
func leftOver(t *testing.T, before []goroutine, wait time.Duration) []goroutine {
	t.Helper()
	ids := make([]string, 0, len(before))
	for _, g := range before {
		ids = append(ids, g.id)
	}
	end := time.Now().Add(wait)
	for {
		var left []goroutine
		for _, g := range goroutines() {
			if !slices.Contains(ids, g.id) && g.runsHere() {
				left = append(left, g)
			}
		}
		if len(left) == 0 || !time.Now().Before(end) {
			return left
		}
		runtime.Gosched()
	}
}

// settleWait is how long a release's goroutine is given to end: a second, stretched for -race and
// load, the load being load.Read's one-minute average over the CPUs, as testenv read it
// before testkit took the reading from its caller. With no reading, as on Windows, the
// wait is stretched for -race alone. A goroutine that ends does so at once, and one that is left is
// named at the end.
func settleWait(t *testing.T) time.Duration {
	t.Helper()
	perCPU := 0.0
	if r, err := load.Read(); err == nil && r.CPUs > 0 {
		perCPU = r.One / float64(r.CPUs)
	}
	return testkit.Scale(t, time.Second, 30, perCPU)
}

// leftOver names a goroutine of this package's code that a test started and left, and no other: one
// there before and one that has ended are not named; a stack is this package's by a frame of its
// code, not by the line that started it, and not by a package beneath its folder.
//
// What it costs when this fails: the release test counts a goroutine that is not the release's and
// fails for one of another package's, or it misses the release's own and a block leaves a goroutine
// behind it with nobody told.
//
//nolint:paralleltest // serial: it reads every goroutine of the process, which a parallel test's would add to
func TestLeftOverNamesAGoroutineOfThisPackageAndNoOther(t *testing.T) {
	stop, there, started := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { // there before the snapshot: not named
		close(there)
		<-stop
	}()
	<-there
	before := goroutines()
	if got := leftOver(t, before, 0); len(got) != 0 {
		t.Fatalf("named with no goroutine started since: %q", got)
	}
	go func() { // this package's code, left running
		close(started)
		<-stop
	}()
	<-started
	if got := leftOver(t, before, 0); len(got) != 1 || !strings.Contains(got[0].stack, self+"TestLeftOverNamesAGoroutineOfThisPackageAndNoOther.func") {
		t.Errorf("a goroutine of this package's left running: named %q, want it alone", got)
	}
	close(stop)
	if got := leftOver(t, before, settleWait(t)); len(got) != 0 {
		t.Errorf("every goroutine started has ended, and %d named: %q", len(got), got)
	}

	const (
		release = "github.com/birdtable/go-proc/sandbox.linuxKernel.release.func1()\n\t/s/kernel_linux.go:271 +0x1\ncreated by github.com/birdtable/go-proc/sandbox.linuxKernel.release in goroutine 6\n\t/s/kernel_linux.go:271 +0x2"
		pipe    = "io.(*pipe).read(...)\n\t/go/io/pipe.go:57\nio.Copy(...)\n\t/go/io/io.go:388\ncreated by github.com/birdtable/go-proc/sandbox.TestX in goroutine 6\n\t/s/x_test.go:9 +0x3"
		beneath = "github.com/birdtable/go-proc/sandbox/cmd/netblock.run()\n\t/s/cmd/netblock/main.go:9 +0x4"
	)
	for stack, want := range map[string]bool{release: true, pipe: false, beneath: false} {
		if got := (goroutine{"9", stack}).runsHere(); got != want {
			t.Errorf("runsHere of %q is %v, want %v", stack, got, want)
		}
	}
	if gs := parseGoroutines("goroutine 7 [running]:\nmain.f()\n\t/a.go:1 +0x1\n\ngoroutine x\n\nnot a goroutine\n"); len(gs) != 1 || gs[0].id != "7" || gs[0].stack != "main.f()\n\t/a.go:1 +0x1" {
		t.Errorf("parseGoroutines read %q", gs)
	}
}
