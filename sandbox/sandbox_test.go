package sandbox

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// fakeKernel records each call the block makes, in order, and fails the one
// named in fail with failWith, or answers abi and fams as set. Its Landlock
// has fixed erratum 2 unless noErratum says otherwise. The probe's
// sockets and file go through before the ruleset, unless before says
// otherwise, and after it meet the refusal of a kernel that holds, unless
// after says otherwise: the file is refused only by a ruleset that handles
// the file rights. dir, when set, is the probe's folder, and temp the
// temporary folder, /tmp when unset.
type fakeKernel struct {
	calls     []string
	fail      string
	failWith  error
	abi       int
	noErratum bool
	fams      [3]int
	dir       string
	temp      string
	handledFS uint64
	scoped    uint64
	rules     []pathRule
	filter    []sockFilter
	// loadR1 and loadErrno are what the seccomp call answers: a thread's id,
	// and an errno.
	loadR1    uintptr
	loadErrno syscall.Errno
	before    *reached
	after     *reached
	probed    []held
	released  held
	ran       []string
	// dryCode and drySaid are what the macOS block's dry run answers, and
	// dried the arguments it was given, the sandbox's own path first.
	dryCode int
	drySaid []byte
	dried   []string
}

// fakeHeld is the probe's sockets and file as the fake makes them.
func fakeHeld() held {
	return held{stream: 7, dgram: 8, dir: "/tmp/netblock-box", streamPath: "/tmp/netblock-box/s", dgramPath: "/tmp/netblock-box/d", filePath: "/tmp/netblock-box/f"}
}

func (k *fakeKernel) step(name string) error {
	k.calls = append(k.calls, name)
	if k.fail == name {
		return k.failWith
	}
	return nil
}

func (k *fakeKernel) noNewPrivs() error { return k.step("noNewPrivs") }
func (k *fakeKernel) closeAtExec(fd int) error {
	if fd != 3 {
		return errors.New("closeAtExec from a descriptor that is not the first above stderr")
	}
	if k.fail == "closeAtExec after dryRun" && slices.Contains(k.calls, "dryRun") {
		k.calls = append(k.calls, "closeAtExec")
		return k.failWith
	}
	return k.step("closeAtExec")
}
func (k *fakeKernel) stdioFamilies() ([3]int, error) { return k.fams, k.step("stdioFamilies") }
func (k *fakeKernel) landlockABI() (int, int, error) {
	errata := landlockErratumSignalScope
	if k.noErratum {
		errata = 0
	}
	return k.abi, errata, k.step("landlockABI")
}
func (k *fakeKernel) procFDs() error { return k.step("procFDs") }
func (k *fakeKernel) tempFolder() (string, error) {
	temp := "/tmp"
	if k.temp != "" {
		temp = k.temp
	}
	return temp, k.step("tempFolder")
}
func (k *fakeKernel) holdOutside() (held, error) {
	if err := k.step("holdOutside"); err != nil {
		return held{}, err
	}
	h := fakeHeld()
	if k.dir != "" {
		h.dir = k.dir
	}
	return h, nil
}
func (k *fakeKernel) restrict(handledFS, scoped uint64, rules []pathRule) error {
	k.handledFS, k.scoped, k.rules = handledFS, scoped, rules
	return k.step("restrict")
}
func (k *fakeKernel) loadFilter(prog []sockFilter) error {
	k.filter = prog
	if err := k.step("loadFilter"); err != nil {
		return err
	}
	return filterLoaded(k.loadR1, k.loadErrno)
}
func (k *fakeKernel) reach(h held) reached {
	k.probed = append(k.probed, h)
	restricted := slices.Contains(k.calls, "restrict")
	_ = k.step("reach")
	switch {
	case !restricted && k.before != nil:
		return *k.before
	case !restricted:
		return reached{}
	case k.after != nil:
		return *k.after
	}
	// The filter's refusal of an AF_INET socket, on every ABI.
	r := reached{inet: syscall.EPERM}
	if k.handledFS&landlockAccessFSABI6 != 0 {
		// Landlock's refusal of the file, where the ruleset handles it.
		r.read, r.write = syscall.EACCES, syscall.EACCES
	}
	if k.handledFS&landlockAccessFSResolveUnix != 0 {
		// Landlock's refusal, where the ruleset handles RESOLVE_UNIX.
		r.stream, r.dgram = syscall.EACCES, syscall.EACCES
		return r
	}
	// The filter's, where it closes the sends.
	r.stream, r.dgram = syscall.EPERM, syscall.EPERM
	return r
}
func (k *fakeKernel) release(h held) error {
	k.released = h
	return k.step("release")
}
func (k *fakeKernel) exec(prog string, argv, _ []string) error {
	k.ran = append([]string{prog}, argv...)
	return k.step("exec")
}
func (k *fakeKernel) dryRun(path string, args []string) (int, []byte, error) {
	k.dried = append([]string{path}, args...)
	if err := k.step("dryRun"); err != nil {
		return -1, nil, err
	}
	return k.dryCode, k.drySaid, nil
}

// member is the program the tests run behind the block, and its arguments.
func member() []string { return []string{"/usr/bin/make", "--no-print-directory", "lint"} }

// linuxBlock runs the Linux block over k on arch, with member.
func linuxBlock(k *fakeKernel, arch string) error {
	return linuxHolding(k, arch, named{})
}

// linuxHolding runs the Linux block over k on arch, with member and the
// files held to n.
func linuxHolding(k *fakeKernel, arch string, n named) error {
	return runLinux(k, arch, n, member()[0], member(), []string{"PATH=/usr/bin"})
}

// macBlock runs the macOS block over k, with member.
func macBlock(k *fakeKernel) error {
	return macHolding(k, named{})
}

// macHolding runs the macOS block over k, with member and the files held to
// n.
func macHolding(k *fakeKernel, n named) error {
	return runMac(k, n, member()[0], member(), []string{"PATH=/usr/bin"})
}

// TestTheLinuxBlockSetsEveryStepThenRunsTheProgram: with every step taken,
// the block asks them in order: it holds its probe's sockets and reaches them
// before the ruleset, scopes out abstract sockets and signals, loads its
// architecture's filter, reaches the sockets again and releases them, and
// execs the program by its absolute path with its arguments. From Landlock ABI 9 the ruleset refuses every Unix
// socket on a path made outside it and the filter lets the calls that reach
// a socket by its address through; below 9 the ruleset handles no file
// access and the filter refuses those calls.
//
// What it costs when this fails: a step is skipped or taken out of order, and a program runs behind a block
// that is not whole.
func TestTheLinuxBlockSetsEveryStepThenRunsTheProgram(t *testing.T) {
	t.Parallel()
	rows := []struct {
		arch       string
		abi        int
		handledFS  uint64
		closeSends bool
	}{
		{"amd64", 6, 0, true},
		{"arm64", 6, 0, true},
		{"amd64", 8, 0, true},
		{"amd64", 9, landlockAccessFSResolveUnix, false},
		{"arm64", 9, landlockAccessFSResolveUnix, false},
		{"arm64", 11, landlockAccessFSResolveUnix, false},
	}
	for _, r := range rows {
		k := &fakeKernel{abi: r.abi, failWith: errors.New("unused"), fail: "exec"}
		err := linuxBlock(k, r.arch)
		// The calls of a block that holds, in the order they are made.
		if want := []string{"noNewPrivs", "closeAtExec", "stdioFamilies", "landlockABI", "holdOutside", "reach", "restrict", "loadFilter", "reach", "release", "exec"}; !slices.Equal(k.calls, want) {
			t.Errorf("%s ABI %d: the steps were %q, want %q", r.arch, r.abi, k.calls, want)
		}
		if want := append([]string{member()[0]}, member()...); !slices.Equal(k.ran, want) {
			t.Errorf("%s ABI %d: the program run was %q, want %q", r.arch, r.abi, k.ran, want)
		}
		if k.scoped != landlockScopeAbstractUnixSocket|landlockScopeSignal {
			t.Errorf("%s ABI %d: the scope was %#x, want the abstract sockets and the signals", r.arch, r.abi, k.scoped)
		}
		if k.handledFS != r.handledFS || k.rules != nil {
			t.Errorf("%s ABI %d: the ruleset handled %#x and granted %v, want %#x and no folder, the files open", r.arch, r.abi, k.handledFS, k.rules, r.handledFS)
		}
		if want, err := filterArches()[r.arch].program(r.closeSends); err != nil || !slices.Equal(k.filter, want) {
			t.Errorf("%s ABI %d: the filter loaded is not the one that closes sends: %t", r.arch, r.abi, r.closeSends)
		}
		if want := []held{fakeHeld(), fakeHeld()}; !slices.Equal(k.probed, want) || k.released != fakeHeld() {
			t.Errorf("%s ABI %d: reached %+v and released %+v, want the sockets held, reached twice and released once", r.arch, r.abi, k.probed, k.released)
		}
		if err == nil || !strings.Contains(err.Error(), "could not be started behind the block") {
			t.Errorf("%s ABI %d: an exec that returned: %v; want it failed", r.arch, r.abi, err)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
}

// TestEachStepOfTheLinuxBlockThatFailsRefusesTheRun: a failed step, Landlock
// that is not built in or not enabled, an ABI below 6, an ABI of 6 or later
// without erratum 2, a standard stream that
// is an inet socket or a socket of no family told, an architecture with no
// filter, a filter load that answers ESRCH or a thread's id, a probe whose
// control does not go through, a probe that answers behind the block or is
// refused with another error than the block's in its mode, which the host may
// give, an AF_INET socket made behind the block on any ABI or refused with
// another error than the filter's EPERM, and a probe not released each refuse the run, with no later step
// taken but the probe's release and no program run.
//
// What it costs when this fails: a failed step is taken for a pass, and the program runs with the network
// open and nobody told.
func TestEachStepOfTheLinuxBlockThatFailsRefusesTheRun(t *testing.T) {
	t.Parallel()
	failed := errors.New("refused by the fake")
	rows := []struct {
		claim string
		k     *fakeKernel
		arch  string
		says  string
		last  string
	}{
		{"no_new_privs fails", &fakeKernel{abi: 6, fail: "noNewPrivs", failWith: failed}, "amd64", "no_new_privs could not be set", "noNewPrivs"},
		{"close at exec fails", &fakeKernel{abi: 6, fail: "closeAtExec", failWith: failed}, "amd64", "close at exec", "closeAtExec"},
		{"the streams cannot be read", &fakeKernel{abi: 6, fail: "stdioFamilies", failWith: failed}, "amd64", "standard streams could not be read", "stdioFamilies"},
		{"stdout is an inet socket", &fakeKernel{abi: 6, fams: [3]int{0, afInet, 0}}, "amd64", "descriptor 1 is of family 2", "stdioFamilies"},
		{"stdin is an inet6 socket", &fakeKernel{abi: 6, fams: [3]int{afInet6, afUnix, 0}}, "arm64", "descriptor 0 is of family 10", "stdioFamilies"},
		{"stderr is a socket of no family told", &fakeKernel{abi: 6, fams: [3]int{0, 0, -1}}, "arm64", "descriptor 2 is of family -1", "stdioFamilies"},
		{"landlock not built in", &fakeKernel{fail: "landlockABI", failWith: syscall.ENOSYS}, "amd64", "landlock is not here", "landlockABI"},
		{"landlock not enabled", &fakeKernel{fail: "landlockABI", failWith: syscall.EOPNOTSUPP}, "amd64", "landlock is not here", "landlockABI"},
		{"landlock ABI 5", &fakeKernel{abi: 5}, "amd64", "ABI 5", "landlockABI"},
		{"landlock ABI 1", &fakeKernel{abi: 1}, "arm64", "ABI 1", "landlockABI"},
		{"landlock ABI 6 without erratum 2", &fakeKernel{abi: 6, noErratum: true}, "amd64", "ABI 6 without erratum 2", "landlockABI"},
		{"landlock ABI 9 without erratum 2", &fakeKernel{abi: 9, noErratum: true}, "arm64", "ABI 9 without erratum 2", "landlockABI"},
		{"the probe's sockets cannot be made", &fakeKernel{abi: 9, fail: "holdOutside", failWith: syscall.ENAMETOOLONG}, "amd64", "probe's Unix sockets outside the block could not be made", "holdOutside"},
		{"the control's connect does not go through", &fakeKernel{abi: 6, before: &reached{stream: syscall.ECONNREFUSED, dgram: nil}}, "amd64", "could not be reached before the block was set", "release"},
		{"the control's datagram does not go through", &fakeKernel{abi: 9, before: &reached{stream: nil, dgram: syscall.EACCES}}, "arm64", "could not be reached before the block was set", "release"},
		{"restrict_self fails", &fakeKernel{abi: 6, fail: "restrict", failWith: syscall.EPERM}, "amd64", "landlock ruleset could not be set", "release"},
		{"the filter load fails", &fakeKernel{abi: 6, fail: "loadFilter", failWith: syscall.EINVAL}, "amd64", "seccomp filter could not be set", "release"},
		{"TSYNC fails with ESRCH", &fakeKernel{abi: 7, loadErrno: syscall.ESRCH, failWith: syscall.ESRCH}, "arm64", "on every thread", "release"},
		{"TSYNC fails with a thread's id and no errno", &fakeKernel{abi: 9, loadR1: 4242, failWith: errFilterUnsynced}, "amd64", "on every thread: " + errFilterUnsynced.Error() + ": the call answered 4242", "release"},
		{"the control's AF_INET socket is not made", &fakeKernel{abi: 9, before: &reached{inet: syscall.EAFNOSUPPORT}, failWith: syscall.EAFNOSUPPORT}, "amd64", "AF_INET socket could not be made before the block was set", "release"},
		{"an AF_INET socket made behind the block on ABI 9", &fakeKernel{abi: 9, after: &reached{stream: syscall.EACCES, dgram: syscall.EACCES}, failWith: errProbeInetAnswered}, "arm64", "the seccomp filter is not set: the probe's AF_INET socket", "release"},
		{"an AF_INET socket made behind the block on ABI 6", &fakeKernel{abi: 6, after: &reached{stream: syscall.EPERM, dgram: syscall.EPERM}, failWith: errProbeInetAnswered}, "amd64", "the seccomp filter is not set: the probe's AF_INET socket", "release"},
		{"an AF_INET socket refused with EACCES, which is not the filter's", &fakeKernel{abi: 9, after: &reached{stream: syscall.EACCES, dgram: syscall.EACCES, inet: syscall.EACCES}}, "amd64", "AF_INET socket was refused with " + syscall.EACCES.Error() + ", but the block's refusal is " + syscall.EPERM.Error(), "release"},
		{"the probe's connect answers on ABI 9", &fakeKernel{abi: 9, after: &reached{stream: nil, dgram: syscall.EACCES}}, "amd64", "does not hold: the probe's connect", "release"},
		{"the probe's datagram answers on ABI 6", &fakeKernel{abi: 6, after: &reached{stream: syscall.EPERM, dgram: nil}}, "arm64", "does not hold: the probe's datagram", "release"},
		{"an ABI 6 refusal of EACCES, which the host gives", &fakeKernel{abi: 6, after: &reached{stream: syscall.EACCES, dgram: syscall.EACCES}}, "amd64", "the block's refusal is " + syscall.EPERM.Error(), "release"},
		{"an ABI 9 refusal of EPERM, which is not Landlock's", &fakeKernel{abi: 9, after: &reached{stream: syscall.EPERM, dgram: syscall.EPERM}}, "arm64", "the block's refusal is " + syscall.EACCES.Error(), "release"},
		{"an ABI 9 datagram refused with EPERM alone", &fakeKernel{abi: 9, after: &reached{stream: syscall.EACCES, dgram: syscall.EPERM}}, "amd64", "datagram to its datagram socket was refused with " + syscall.EPERM.Error(), "release"},
		{"the probe's path is gone", &fakeKernel{abi: 9, after: &reached{stream: syscall.ENOENT, dgram: syscall.ENOENT}}, "amd64", "was refused with " + syscall.ENOENT.Error(), "release"},
		{"the probe is refused by the listener's backlog", &fakeKernel{abi: 6, after: &reached{stream: syscall.ECONNREFUSED, dgram: syscall.EPERM}}, "arm64", "was refused with " + syscall.ECONNREFUSED.Error(), "release"},
		{"the probe's sockets cannot be removed", &fakeKernel{abi: 9, fail: "release", failWith: syscall.EBUSY}, "amd64", "probe's Unix sockets could not be removed", "release"},
		{"an architecture with no filter", &fakeKernel{abi: 6}, "riscv64", "no seccomp filter for the architecture", ""},
		{"an architecture not named", &fakeKernel{abi: 6}, "", "no seccomp filter for the architecture", ""},
	}
	for _, r := range rows {
		err := linuxBlock(r.k, r.arch)
		if err == nil || !strings.Contains(err.Error(), r.says) || !strings.Contains(err.Error(), "the program is not run") {
			t.Errorf("%s: %v; want it refused, saying %q", r.claim, err, r.says)
		}
		if r.k.ran != nil || slices.Contains(r.k.calls, "exec") {
			t.Errorf("%s: the program was run: %q", r.claim, r.k.ran)
		}
		refusedAsNoBlock(t, r.claim, err, r.k.failWith)
		if last := lastOf(r.k.calls); last != r.last {
			t.Errorf("%s: the last step taken was %q, want %q, then none", r.claim, last, r.last)
		}
		// A probe made is released whatever failed after it.
		if slices.Contains(r.k.calls, "holdOutside") && r.k.fail != "holdOutside" && r.k.released != fakeHeld() {
			t.Errorf("%s: the probe socket was not released", r.claim)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
}

// refusedAsNoBlock fails t unless err, a refusal that left the block unset,
// matches ErrNoBlock, still matches the step's own error when the fake gave
// one, and says nothing of ErrNoBlock's text.
func refusedAsNoBlock(t *testing.T, claim string, err, step error) {
	t.Helper()
	if !errors.Is(err, ErrNoBlock) {
		t.Errorf("%s: %v does not match ErrNoBlock", claim, err)
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("%s: %v does not match errors.ErrUnsupported, which ErrNoBlock wraps", claim, err)
	}
	if step != nil && !errors.Is(err, step) {
		t.Errorf("%s: %v no longer matches the step's own error, %v", claim, err, step)
	}
	// Only a system with no block at all is ErrNoSystem; a step this host refused is not.
	if want := errors.Is(step, errNoSystem); errors.Is(err, ErrNoSystem) != want {
		t.Errorf("%s: %v matches ErrNoSystem %v, want %v", claim, err, !want, want)
	}
	if err != nil && strings.Contains(err.Error(), ErrNoBlock.Error()) {
		t.Errorf("%s: %q carries ErrNoBlock's text, want the step's own", claim, err)
	}
}

func lastOf(calls []string) string {
	if len(calls) == 0 {
		return ""
	}
	return calls[len(calls)-1]
}

// TestTheMacBlockChecksTheStreamsThenRunsTheSandbox: the macOS block marks
// the descriptors and reads the streams, takes no Linux step, sets its
// profile in a dry run of the sandbox around /usr/bin/true, marks the
// descriptors again, and execs the
// system sandbox by its absolute path with the profile that denies the
// network, then the program and its arguments.
//
// What it costs when this fails: the sandbox is started with a profile that lets the network through, or
// without the check of the streams, and the program has the network.
func TestTheMacBlockChecksTheStreamsThenRunsTheSandbox(t *testing.T) {
	t.Parallel()
	k := &fakeKernel{fail: "exec", failWith: errors.New("unused")}
	err := macBlock(k)
	if want := []string{"closeAtExec", "stdioFamilies", "dryRun", "closeAtExec", "exec"}; !slices.Equal(k.calls, want) {
		t.Errorf("the steps were %q, want %q", k.calls, want)
	}
	if want := []string{sandboxProgram, "-p", macosProfile, macNoop}; !slices.Equal(k.dried, want) {
		t.Errorf("the dry run was %q, want %q, the profile around a program that does nothing", k.dried, want)
	}
	want := append([]string{"/usr/bin/sandbox-exec", "/usr/bin/sandbox-exec", "-p", `(version 1)(allow default)(deny network*)(deny file-ioctl (ioctl-command #x80017472))(deny mach-lookup)(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))(deny mach-priv-task-port)`}, member()...)
	if !slices.Equal(k.ran, want) {
		t.Errorf("the program run was %q, want %q", k.ran, want)
	}
	if err == nil || !strings.Contains(err.Error(), sandboxProgram+" could not be started, so no block is set and "+member()[0]+" is not run") {
		t.Errorf("an exec that returned: %v; want it failed, naming the sandbox and the program", err)
	}
}

// TestTheMacBlockRefusesTheRunWhenTheDescriptorsCannotBeMarkedAgain: a
// marking of the descriptors that fails after the dry run, which passed,
// refuses the run with ErrNoBlock and the kernel's error, naming the program,
// and the sandbox is never exec'd.
//
// What it costs when this fails: a descriptor opened during the dry run is
// left open in the program when the marking after it fails, read and written
// behind the block.
func TestTheMacBlockRefusesTheRunWhenTheDescriptorsCannotBeMarkedAgain(t *testing.T) {
	t.Parallel()
	k := &fakeKernel{fail: "closeAtExec after dryRun", failWith: syscall.EBADF}
	err := macBlock(k)
	if want := []string{"closeAtExec", "stdioFamilies", "dryRun", "closeAtExec"}; !slices.Equal(k.calls, want) {
		t.Errorf("the steps were %q, want %q", k.calls, want)
	}
	if err == nil || !strings.Contains(err.Error(), "could not be marked to close at exec again after the dry run, so no block is set and "+member()[0]+" is not run") {
		t.Errorf("a marking that failed after the dry run: %v; want it refused, naming the program", err)
	}
	if k.ran != nil {
		t.Errorf("the sandbox was run with the program: %q", k.ran)
	}
	refusedAsNoBlock(t, "a marking that failed after the dry run", err, k.failWith)
}

// TestEachStepOfTheMacBlockThatFailsRefusesTheRun: a descriptor not marked,
// streams not read, and a standard stream that is a socket of another family
// than AF_UNIX each refuse the run, and the sandbox is not started.
//
// What it costs when this fails: a step that failed is ignored, and a program runs on a Mac with a
// descriptor unmarked or a stream open to the network.
func TestEachStepOfTheMacBlockThatFailsRefusesTheRun(t *testing.T) {
	t.Parallel()
	failed := errors.New("refused by the fake")
	rows := []struct {
		claim string
		k     *fakeKernel
		says  string
		last  string
	}{
		{"close at exec fails", &fakeKernel{fail: "closeAtExec", failWith: failed}, "close at exec", "closeAtExec"},
		{"the streams cannot be read", &fakeKernel{fail: "stdioFamilies", failWith: failed}, "standard streams could not be read", "stdioFamilies"},
		{"stdin is an inet socket", &fakeKernel{fams: [3]int{afInet, 0, 0}}, "descriptor 0 is of family 2", "stdioFamilies"},
		{"stderr is a socket of no family told", &fakeKernel{fams: [3]int{0, afUnix, -1}}, "descriptor 2 is of family -1", "stdioFamilies"},
	}
	for _, r := range rows {
		err := macBlock(r.k)
		if err == nil || !strings.Contains(err.Error(), r.says) || !strings.Contains(err.Error(), "the program is not run") {
			t.Errorf("%s: %v; want it refused, saying %q", r.claim, err, r.says)
		}
		if r.k.ran != nil || r.k.dried != nil || slices.Contains(r.k.calls, "exec") {
			t.Errorf("%s: the sandbox was started: %q, dry run %q", r.claim, r.k.ran, r.k.dried)
		}
		refusedAsNoBlock(t, r.claim, err, r.k.failWith)
		if last := lastOf(r.k.calls); last != r.last {
			t.Errorf("%s: the last step taken was %q, want %q, then none", r.claim, last, r.last)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
}

// TestRunTakesAProgramByItsAbsolutePathOnly: a program that is not named by
// its absolute path is a usage error from Run and RunHolding on every
// system, before any step is set, so the tests' own process is never
// blocked. The error names the program and speaks of no command line: no
// command and no --.
//
// What it costs when this fails: a program that is not named by its absolute path sets the block on the
// tests' own process, or runs a program found by a search path, or a library caller is told of a
// command line it never wrote.
func TestRunTakesAProgramByItsAbsolutePathOnly(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw")
	paths := []string{"", "make", "./make", "bin/make", "--"}
	ran := 0
	for _, path := range paths {
		for call, err := range map[string]error{
			"Run":        Run(path, []string{"lint"}, nil),
			"RunHolding": RunHolding(Folders{ReadWrite: []string{dir + "/rw"}}, path, nil, nil),
		} {
			ran++
			if !errors.Is(err, ErrUsage) {
				t.Errorf("%s %q: %v, want the usage", call, path, err)
				continue
			}
			if errors.Is(err, ErrNoBlock) {
				t.Errorf("%s %q: the usage matches ErrNoBlock, so a caller would read a program it does not take as a block refused", call, path)
			}
			if says := err.Error(); !strings.Contains(says, strconv.Quote(path)) || strings.Contains(says, "command") || strings.Contains(says, "--") && path != "--" {
				t.Errorf("%s %q: says %q, want the program named and no command line", call, path, says)
			}
		}
	}
	if ran < 2*len(paths) {
		t.Fatalf("ran %d calls", ran)
	}
}

// TestAProgramThatCannotBeStartedBehindTheLinuxBlockIsNotErrNoBlock: on
// Linux the block is set before the exec, so a program the exec cannot start
// (not there, not allowed, not a program) or an exec that returns is the
// program's failure, not the block's. It does not match ErrNoBlock, it still
// matches the exec's own error, and its text names the program.
//
// What it costs when this fails: a caller that falls back when the block
// cannot be set reads a program that is missing as a machine with no block,
// and reports "no sandbox" for a typo in a path, or retries it elsewhere.
func TestAProgramThatCannotBeStartedBehindTheLinuxBlockIsNotErrNoBlock(t *testing.T) {
	t.Parallel()
	rows := []struct {
		claim string
		with  error
	}{
		{"the program is not there", syscall.ENOENT},
		{"the program may not be run", syscall.EACCES},
		{"the file is not a program", syscall.ENOEXEC},
		{"the exec returned", nil},
	}
	for _, r := range rows {
		k := &fakeKernel{abi: 9, fail: "exec", failWith: r.with}
		err := linuxBlock(k, "amd64")
		if err == nil || !strings.Contains(err.Error(), member()[0]+" could not be started behind the block") {
			t.Errorf("%s: %v; want it failed, naming the program", r.claim, err)
		}
		if errors.Is(err, ErrNoBlock) {
			t.Errorf("%s: %v matches ErrNoBlock, but the block was set", r.claim, err)
		}
		if r.with != nil && !errors.Is(err, r.with) {
			t.Errorf("%s: %v does not match the exec's own error, %v", r.claim, err, r.with)
		}
		if !slices.Contains(k.calls, "release") || lastOf(k.calls) != "exec" {
			t.Errorf("%s: the steps were %q, want the block set and then the exec", r.claim, k.calls)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("ran %d rows, want 4", len(rows))
	}
}

// TestTheSandboxThatCannotBeStartedOnAMacIsErrNoBlock: on macOS the block is
// the system sandbox, and the exec is the sandbox's, so an exec that fails
// or returns leaves the program with no block at all. It matches ErrNoBlock,
// still matches the exec's own error, and its text is as it was. A program
// the sandbox cannot start is the sandbox's exit, after the exec, which Run
// never sees.
//
// What it costs when this fails: a Mac whose sandbox is gone reads as a
// program that failed, and a caller that would report "no sandbox" and stop
// reports a tool's failure instead.
func TestTheSandboxThatCannotBeStartedOnAMacIsErrNoBlock(t *testing.T) {
	t.Parallel()
	rows := []struct {
		claim string
		with  error
	}{
		{"the sandbox is not there", syscall.ENOENT},
		{"the sandbox may not be run", syscall.EACCES},
		{"the exec returned", nil},
	}
	for _, r := range rows {
		k := &fakeKernel{fail: "exec", failWith: r.with}
		err := macBlock(k)
		if err == nil || !strings.Contains(err.Error(), sandboxProgram+" could not be started, so no block is set and "+member()[0]+" is not run") {
			t.Errorf("%s: %v; want it failed, naming the sandbox and the program", r.claim, err)
		}
		if lastOf(k.calls) != "exec" || lastOf(k.ran) == "" || k.ran[0] != sandboxProgram {
			t.Errorf("%s: the steps were %q and the run %q, want the sandbox's exec", r.claim, k.calls, k.ran)
		}
		refusedAsNoBlock(t, r.claim, err, r.with)
	}
	if len(rows) != 3 {
		t.Fatalf("ran %d rows, want 3", len(rows))
	}
}

// TestAProfileTheMacSandboxDoesNotSetRefusesTheRunBeforeItsExec: the macOS
// block sets its profile in a dry run of the sandbox first, and a dry run
// that does not exit 0, or cannot be started, refuses the run before the
// exec: it matches ErrNoBlock, the sandbox is never run with the program, and
// the error gives the sandbox's code and its own first line, and names the
// program. After the exec this process is the sandbox, and a sandbox that
// refused the profile there would exit 65, a code the program could give too.
//
// What it costs when this fails: a profile the sandbox refuses reads as the
// program's own failing exit, and a caller reports "the tool failed" where
// the tool never ran.
func TestAProfileTheMacSandboxDoesNotSetRefusesTheRunBeforeItsExec(t *testing.T) {
	t.Parallel()
	rows := []struct {
		claim string
		k     *fakeKernel
		says  string
	}{
		{"a profile the sandbox refuses", &fakeKernel{dryCode: 65, drySaid: []byte("sandbox-exec: unbound variable: bogus at <input string>, line 1, column 13\n\nBacktrace: \n<input string>:1:13:\n")},
			`it exited 65: "sandbox-exec: unbound variable: bogus at <input string>, line 1, column 13"`},
		{"a parameter the sandbox refuses", &fakeKernel{dryCode: 65, drySaid: []byte("sandbox-exec: invalid data type of path filter; expected pattern, got boolean\n")},
			`it exited 65: "sandbox-exec: invalid data type of path filter; expected pattern, got boolean"`},
		{"a flag the sandbox refuses", &fakeKernel{dryCode: 64, drySaid: []byte("/usr/bin/sandbox-exec: option requires an argument -- D\nUsage: sandbox-exec [options] command [args]\n")},
			`it exited 64: "/usr/bin/sandbox-exec: option requires an argument -- D"`},
		{"a dry run its bound, its output cap or a signal ended", &fakeKernel{dryCode: -1, drySaid: []byte("sandbox-exec: what it wrote first\n")}, `it exited -1: "sandbox-exec: what it wrote first"`},
		{"a dry run that exits 1 and says nothing but blanks", &fakeKernel{dryCode: 1, drySaid: []byte("  \n")}, `it exited 1: ""`},
		{"a dry run that cannot be started", &fakeKernel{fail: "dryRun", failWith: syscall.ENOENT}, "could not be started for its dry run of the profile"},
	}
	for _, r := range rows {
		err := macBlock(r.k)
		if err == nil || !strings.Contains(err.Error(), r.says) || !strings.Contains(err.Error(), "so no block is set and "+member()[0]+" is not run") {
			t.Errorf("%s: %v; want it refused before the exec, saying %q and naming the program", r.claim, err, r.says)
		}
		if r.k.ran != nil || slices.Contains(r.k.calls, "exec") {
			t.Errorf("%s: the sandbox was run with the program: %q", r.claim, r.k.ran)
		}
		if last := lastOf(r.k.calls); last != "dryRun" {
			t.Errorf("%s: the last step taken was %q, want the dry run, then none", r.claim, last)
		}
		refusedAsNoBlock(t, r.claim, err, r.k.failWith)
	}
	if len(rows) != 6 {
		t.Fatalf("ran %d rows, want 6", len(rows))
	}
}

// TestASystemWithNoBlockRefusesWithErrNoBlock: where the system gives no
// block, Run refuses with the text it always had, and the refusal matches
// ErrNoBlock and ErrNoSystem. runNone is Run's answer there, so this runs on every system.
// Every step refusal the other tests make is held by refusedAsNoBlock to match ErrNoSystem never.
//
// What it costs when this fails: on a system with no block a caller cannot
// tell that the block was the reason, and reads the refusal as the program's,
// or cannot tell a system with no block from a host that refused a step.
func TestASystemWithNoBlockRefusesWithErrNoBlock(t *testing.T) {
	t.Parallel()
	err := runNone()
	const was = "this system gives no network block netblock can set (Linux sets its own filter, macOS runs /usr/bin/sandbox-exec), so the program is not run with the network open"
	if err == nil || err.Error() != was {
		t.Errorf("a system with no block refused with %v, want %q", err, was)
	}
	refusedAsNoBlock(t, "a system with no block", err, errNoSystem)
}
