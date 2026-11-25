package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// realTemp is a new folder of the test's by its real path, with no symbolic
// link in it, and in it the folders names, made.
func realTemp(t *testing.T, names ...string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRunHoldingTakesItsFoldersByTheirRealPathsOnly: a folder that is not an
// absolute path, does not exist or is a file, one named twice in a list,
// across the lists or through a symbolic link, a read-only folder beneath or
// equal to a read-write one, and no folder at all are each ErrUsage before
// anything is set, and none of them matches ErrNoBlock. The folders that
// pass come back by their real paths, in their lists, in their order; a
// read-write folder beneath a read-only one is a narrower grant and passes.
//
// What it costs when this fails: a folder the caller did not mean is granted, a symbolic link moves a
// grant after the check, a read-only folder can be written, or RunHolding with no folder runs with
// every file open while the caller believes them held.
func TestRunHoldingTakesItsFoldersByTheirRealPathsOnly(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw", "ro", "ro2", "rw/inner", "ro/inner")
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Windows makes a symbolic link only for an account with the privilege to,
	// so there the rows that name one are run only where the link was made.
	linked := true
	if err := os.Symlink(filepath.Join(dir, "rw"), filepath.Join(dir, "link")); err != nil {
		if runtime.GOOS != "windows" { //nolint:forbidigo // arch.ports.1: Windows makes a symbolic link only for an account with the privilege, so only there may the link be missing
			t.Fatal(err)
		}
		linked = false
		t.Logf("no symbolic link here, so the rows that name one are left out: %v", err)
	}
	at := func(n string) string { return filepath.Join(dir, n) }
	// root is the root folder of dir's volume, and the program an absolute
	// path on every system, "/" and a file under dir on Unix, a drive's root
	// and a file under dir on Windows.
	root := filepath.VolumeName(dir) + string(filepath.Separator)
	prog := at("program")
	// checked is RunHolding's own order: the program, then the folders.
	checked := func(f Folders, path string) (named, error) {
		if _, err := usage(path, []string{"lint"}); err != nil {
			return named{}, err
		}
		return holding(f)
	}
	bad := []struct {
		claim   string
		folders Folders
		path    string
		says    string
	}{
		{"a relative folder", Folders{ReadWrite: []string{"rw"}}, prog, "not an absolute path"},
		{"a relative read-only folder", Folders{ReadWrite: []string{at("rw")}, ReadOnly: []string{"./ro"}}, prog, "not an absolute path"},
		{"a folder that does not exist", Folders{ReadWrite: []string{at("gone")}}, prog, "cannot be held"},
		{"a read-only folder that does not exist", Folders{ReadOnly: []string{at("gone")}}, prog, "cannot be held"},
		{"a file", Folders{ReadOnly: []string{at("file")}}, prog, "it is not a folder"},
		{"a folder named twice", Folders{ReadWrite: []string{at("rw"), at("rw")}}, prog, "named twice"},
		{"a folder named twice with a trailing slash", Folders{ReadOnly: []string{at("ro"), at("ro") + "/"}}, prog, "named twice"},
		{"a folder in both lists", Folders{ReadWrite: []string{at("rw")}, ReadOnly: []string{at("rw")}}, prog, "named twice"},
		{"a folder named again through a link", Folders{ReadWrite: []string{at("rw")}, ReadOnly: []string{at("link")}}, prog, "named twice"},
		{"a read-only folder beneath a read-write one", Folders{ReadWrite: []string{at("rw")}, ReadOnly: []string{at("rw/inner")}}, prog, "beneath the read-write folder"},
		{"a read-only folder beneath a read-write root", Folders{ReadWrite: []string{root}, ReadOnly: []string{at("ro")}}, prog, "beneath the read-write folder"},
		{"no folder", Folders{}, prog, "names no folder"},
		{"empty lists", Folders{ReadWrite: []string{}, ReadOnly: []string{}}, prog, "names no folder"},
		{"a program not by its absolute path", Folders{ReadWrite: []string{at("rw")}}, "make", `the program "make" is not named by its absolute path`},
	}
	ran := 0
	for _, r := range bad {
		if !linked && strings.Contains(r.claim, "link") {
			continue
		}
		ran++
		n, err := checked(r.folders, r.path)
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v; want ErrUsage, saying %q", r.claim, err, r.says)
		}
		if errors.Is(err, ErrNoBlock) {
			t.Errorf("%s: %v matches ErrNoBlock, but nothing was set", r.claim, err)
		}
		if n.holds() {
			t.Errorf("%s: folders came back with the error: %+v", r.claim, n)
		}
	}
	if ran < len(bad)-1 {
		t.Fatalf("ran %d of the %d rows", ran, len(bad))
	}
	good := []struct {
		claim   string
		folders Folders
		want    named
	}{
		{"read-write alone", Folders{ReadWrite: []string{at("rw")}}, named{readWrite: []string{at("rw")}}},
		{"read-only alone", Folders{ReadOnly: []string{at("ro"), at("ro2")}}, named{readOnly: []string{at("ro"), at("ro2")}}},
		{"a link, by its real path", Folders{ReadWrite: []string{at("link")}, ReadOnly: []string{at("ro")}}, named{readWrite: []string{at("rw")}, readOnly: []string{at("ro")}}},
		{"a read-write folder beneath a read-only one", Folders{ReadWrite: []string{at("ro/inner")}, ReadOnly: []string{at("ro")}}, named{readWrite: []string{at("ro/inner")}, readOnly: []string{at("ro")}}},
		{"a name with dots", Folders{ReadWrite: []string{at("ro") + "/../rw/."}}, named{readWrite: []string{at("rw")}}},
		{"the root, read-only", Folders{ReadWrite: []string{at("rw")}, ReadOnly: []string{root}}, named{readWrite: []string{at("rw")}, readOnly: []string{root}}},
	}
	ran = 0
	for _, r := range good {
		if !linked && strings.Contains(r.claim, "link") {
			continue
		}
		ran++
		n, err := checked(r.folders, prog)
		if err != nil || !slices.Equal(n.readWrite, r.want.readWrite) || !slices.Equal(n.readOnly, r.want.readOnly) {
			t.Errorf("%s: %+v, %v; want %+v", r.claim, n, err, r.want)
		}
	}
	if ran < len(good)-1 {
		t.Fatalf("ran %d of the %d rows", ran, len(good))
	}
}

// TestTheFileRightsAreThoseOfTheKernelsUapi: the rights the ruleset handles
// and grants are the kernel's numbers: every right of ABI 6 is bits 0 to
// 15, reading and running are EXECUTE, READ_FILE and READ_DIR, and
// RESOLVE_UNIX, bit 16, is in neither, so a folder never grants a Unix
// socket made outside.
//
// What it costs when this fails: a right is handled that the kernel does not know, and the ruleset is
// refused, or a right is left unhandled and open everywhere, or a read-only folder can be written.
func TestTheFileRightsAreThoseOfTheKernelsUapi(t *testing.T) {
	t.Parallel()
	if landlockAccessFSABI6 != 0xffff {
		t.Errorf("every right of ABI 6 is %#x, want 0xffff", landlockAccessFSABI6)
	}
	if landlockAccessFSRead != 0x1|0x4|0x8 {
		t.Errorf("reading and running is %#x, want EXECUTE, READ_FILE and READ_DIR, 0xd", landlockAccessFSRead)
	}
	if landlockAccessFSResolveUnix&(landlockAccessFSABI6|landlockAccessFSRead) != 0 {
		t.Errorf("RESOLVE_UNIX %#x is among the rights a folder grants", landlockAccessFSResolveUnix)
	}
	n := named{readWrite: []string{"/w1", "/w2"}, readOnly: []string{"/r"}}
	want := []pathRule{{"/w1", 0xffff}, {"/w2", 0xffff}, {"/r", 0xd}}
	if got := n.rules(); !slices.Equal(got, want) {
		t.Errorf("the rules are %+v, want %+v", got, want)
	}
	if got := (named{}).rules(); got != nil {
		t.Errorf("no folder gives the rules %+v, want none", got)
	}
}

// TestTheLinuxBlockHoldingFilesGrantsThemBeneathTheFoldersAlone: with
// folders, the ruleset handles every file right of ABI 6, with RESOLVE_UNIX
// from ABI 9, and grants them beneath the folders alone: every right
// beneath a read-write folder and reading and running beneath a read-only
// one. The probe's file is read and written before the ruleset and refused
// behind it, the steps are those of the block that leaves the files open,
// and the program is run.
//
// What it costs when this fails: the ruleset handles no file right and every file stays open, or a
// folder is granted more than its list says, and the program reads or writes outside them.
func TestTheLinuxBlockHoldingFilesGrantsThemBeneathTheFoldersAlone(t *testing.T) {
	t.Parallel()
	n := named{readWrite: []string{"/work"}, readOnly: []string{"/usr/bin", "/lib"}}
	rules := []pathRule{{"/work", landlockAccessFSABI6}, {"/usr/bin", landlockAccessFSRead}, {"/lib", landlockAccessFSRead}}
	rows := []struct {
		arch      string
		abi       int
		handledFS uint64
	}{
		{"amd64", 6, landlockAccessFSABI6},
		{"arm64", 8, landlockAccessFSABI6},
		{"amd64", 9, landlockAccessFSABI6 | landlockAccessFSResolveUnix},
		{"arm64", 11, landlockAccessFSABI6 | landlockAccessFSResolveUnix},
	}
	for _, r := range rows {
		k := &fakeKernel{abi: r.abi, failWith: errors.New("unused"), fail: "exec"}
		err := linuxHolding(k, r.arch, n)
		if want := []string{"noNewPrivs", "closeAtExec", "stdioFamilies", "landlockABI", "holdOutside", "reach", "restrict", "loadFilter", "reach", "release", "exec"}; !slices.Equal(k.calls, want) {
			t.Errorf("%s ABI %d: the steps were %q, want %q", r.arch, r.abi, k.calls, want)
		}
		if k.handledFS != r.handledFS || !slices.Equal(k.rules, rules) {
			t.Errorf("%s ABI %d: the ruleset handled %#x and granted %+v, want %#x and %+v", r.arch, r.abi, k.handledFS, k.rules, r.handledFS, rules)
		}
		if want := append([]string{member()[0]}, member()...); !slices.Equal(k.ran, want) {
			t.Errorf("%s ABI %d: the program run was %q, want %q", r.arch, r.abi, k.ran, want)
		}
		if err == nil || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), "could not be started behind the block") {
			t.Errorf("%s ABI %d: an exec that returned: %v; want it failed, behind a block that was set", r.arch, r.abi, err)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
}

// TestEachFileStepOfTheLinuxBlockThatFailsRefusesTheRun: holding files, a
// probe file that cannot be read or written before the ruleset, one read or
// written behind it, one refused with another error than Landlock's EACCES,
// which the host may give, a probe folder beneath or equal to a folder the
// block grants, and a ruleset whose rules are refused each refuse the run,
// matching ErrNoBlock, with the probe released and no program run. Without
// folders, the same file answering behind the block is not the block's to
// refuse, and the program runs.
//
// What it costs when this fails: a block that does not hold the files runs the program anyway, and it
// reads and writes outside the folders while its caller believes them held.
func TestEachFileStepOfTheLinuxBlockThatFailsRefusesTheRun(t *testing.T) {
	t.Parallel()
	// The folders and the probe's are Linux paths, slash paths on every host,
	// as linuxHolding runs the Linux block's logic on a fake kernel: Windows
	// volume paths are never handed to it, and probeOutside compares by "/".
	work, usr := "/work", "/usr"
	inWork, inUsr := "/work/tmp/netblock-1", "/usr/tmp/netblock-1"
	q := strconv.Quote
	n := named{readWrite: []string{work}, readOnly: []string{usr}}
	rows := []struct {
		claim string
		k     *fakeKernel
		arch  string
		says  string
		last  string
	}{
		{"the control's read does not go through", &fakeKernel{abi: 6, before: &reached{read: syscall.EACCES}}, "amd64", "would not be the block's: the read, " + syscall.EACCES.Error() + ", and the write, it went through", "release"},
		{"the control's write does not go through", &fakeKernel{abi: 9, before: &reached{write: syscall.EROFS}}, "arm64", "file could not be read and written before the block was set", "release"},
		{"the file is read behind the block", &fakeKernel{abi: 9, after: &reached{stream: syscall.EACCES, dgram: syscall.EACCES, inet: syscall.EPERM, read: nil, write: syscall.EACCES}}, "amd64", "does not hold the files: the probe's read of its file", "release"},
		{"the file is written behind the block", &fakeKernel{abi: 6, after: &reached{stream: syscall.EPERM, dgram: syscall.EPERM, inet: syscall.EPERM, read: syscall.EACCES, write: nil}}, "arm64", "does not hold the files: the probe's write of its file", "release"},
		{"the file is neither refused", &fakeKernel{abi: 6, after: &reached{stream: syscall.EPERM, dgram: syscall.EPERM, inet: syscall.EPERM}}, "amd64", "does not hold the files", "release"},
		{"the read refused with EPERM, which is not Landlock's", &fakeKernel{abi: 9, after: &reached{stream: syscall.EACCES, dgram: syscall.EACCES, inet: syscall.EPERM, read: syscall.EPERM, write: syscall.EACCES}}, "amd64", "read of its file was refused with " + syscall.EPERM.Error() + ", but the block's refusal is " + syscall.EACCES.Error(), "release"},
		{"the write refused with EROFS, which the host gives", &fakeKernel{abi: 6, after: &reached{stream: syscall.EPERM, dgram: syscall.EPERM, inet: syscall.EPERM, read: syscall.EACCES, write: syscall.EROFS}}, "arm64", "write of its file was refused with " + syscall.EROFS.Error(), "release"},
		{"the probe's folder beneath a read-write folder", &fakeKernel{abi: 9, dir: inWork}, "amd64", "in TMPDIR: " + q(inWork) + " is beneath " + q(work), "release"},
		{"the probe's folder beneath a read-only folder", &fakeKernel{abi: 6, dir: inUsr}, "arm64", "is beneath " + q(usr), "release"},
		{"the probe's folder is a folder granted", &fakeKernel{abi: 6, dir: work}, "amd64", q(work) + " is beneath " + q(work), "release"},
		{"the rules are refused", &fakeKernel{abi: 6, fail: "restrict", failWith: syscall.EBADF}, "amd64", "landlock ruleset could not be set: " + syscall.EBADF.Error(), "release"},
	}
	for _, r := range rows {
		err := linuxHolding(r.k, r.arch, n)
		if err == nil || !strings.Contains(err.Error(), r.says) || !strings.Contains(err.Error(), "the program is not run") {
			t.Errorf("%s: %v; want it refused, saying %q", r.claim, err, r.says)
		}
		if r.k.ran != nil || slices.Contains(r.k.calls, "exec") {
			t.Errorf("%s: the program was run: %q", r.claim, r.k.ran)
		}
		refusedAsNoBlock(t, r.claim, err, r.k.failWith)
		if last := lastOf(r.k.calls); last != r.last || r.k.released.filePath != fakeHeld().filePath {
			t.Errorf("%s: the last step taken was %q and %+v released, want %q, the probe released, then none", r.claim, last, r.k.released, r.last)
		}
		// A probe folder in a granted folder is refused before the ruleset.
		if strings.Contains(r.claim, "probe's folder") && slices.Contains(r.k.calls, "restrict") {
			t.Errorf("%s: the ruleset was set over a probe it could not refuse", r.claim)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
	open := &fakeKernel{abi: 9, dir: inWork, before: &reached{read: syscall.EACCES}, after: &reached{stream: syscall.EACCES, dgram: syscall.EACCES, inet: syscall.EPERM}, fail: "exec", failWith: errors.New("unused")}
	if err := linuxBlock(open, "amd64"); errors.Is(err, ErrNoBlock) || open.ran == nil {
		t.Errorf("without folders, the probe's file decided the run: %v", err)
	}
}

// TestTheMacBlockHoldingFilesPassesEachFolderAsAParameter: with folders, the
// profile denies every file read and write, gives back the root folder's own
// list, and grants reading beneath each read-only folder and reading and
// writing beneath each read-write one, each by a (param ...) the sandbox's
// -D sets, the network, ioctl and mach-lookup clauses kept whole. Without
// folders the profile is the one that leaves the files open, with no
// parameter.
//
// What it costs when this fails: the profile leaves the files open, grants a folder more than its list,
// or carries a folder's name in its text, where a name could change what it allows.
func TestTheMacBlockHoldingFilesPassesEachFolderAsAParameter(t *testing.T) {
	t.Parallel()
	head := `(version 1)(allow default)(deny network*)(deny file-read* file-write*)(allow file-read-data (literal "/"))`
	tail := `(deny file-ioctl (ioctl-command #x80017472))(deny mach-lookup)(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))(deny mach-priv-task-port)`
	rows := []struct {
		claim   string
		n       named
		profile string
		params  []string
	}{
		{"no folder", named{}, `(version 1)(allow default)(deny network*)` + tail, nil},
		{"read-write and read-only", named{readWrite: []string{"/w1", "/w2"}, readOnly: []string{"/r"}},
			head + `(allow file-read* (subpath (param "RO0")))(allow file-read* file-write* (subpath (param "RW0")) (subpath (param "RW1")))` + tail,
			[]string{"-D", "RO0=/r", "-D", "RW0=/w1", "-D", "RW1=/w2"}},
		{"read-write alone", named{readWrite: []string{"/w"}},
			head + `(allow file-read* file-write* (subpath (param "RW0")))` + tail,
			[]string{"-D", "RW0=/w"}},
		{"read-only alone", named{readOnly: []string{"/r1", "/r2"}},
			head + `(allow file-read* (subpath (param "RO0")) (subpath (param "RO1")))` + tail,
			[]string{"-D", "RO0=/r1", "-D", "RO1=/r2"}},
	}
	for _, r := range rows {
		k := &fakeKernel{fail: "exec", failWith: errors.New("unused")}
		err := macHolding(k, r.n)
		want := append(append(append([]string{sandboxProgram, sandboxProgram}, r.params...), "-p", r.profile), member()...)
		if !slices.Equal(k.ran, want) {
			t.Errorf("%s: the sandbox ran %q, want %q", r.claim, k.ran, want)
		}
		dry := append(append(append([]string{sandboxProgram}, r.params...), "-p", r.profile), macNoop)
		if !slices.Equal(k.dried, dry) {
			t.Errorf("%s: the dry run was %q, want %q, the same profile and parameters as the exec", r.claim, k.dried, dry)
		}
		if !errors.Is(err, ErrNoBlock) {
			t.Errorf("%s: an exec of the sandbox that returned: %v; want ErrNoBlock", r.claim, err)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
}

// TestAFolderNameNeverChangesTheMacProfile: a folder whose name holds a
// quote, a paren, a newline, an equals sign or a clause of its own gives the
// very profile a plain name gives, and its name only in its parameter,
// whole.
//
// What it costs when this fails: a folder name is spliced into the profile, and a folder named for it
// grants itself every file on the machine, or the network.
func TestAFolderNameNeverChangesTheMacProfile(t *testing.T) {
	t.Parallel()
	plain, _ := macosHolding(named{readWrite: []string{"/w"}, readOnly: []string{"/r"}})
	names := []string{
		`/tmp/a"b`,
		`/tmp/a)(allow file-write* (subpath "/"))(b`,
		"/tmp/a\nb",
		`/tmp/a=b`,
		`/tmp/x")) (allow network*) ((param "y`,
		`/tmp/a\"b`,
	}
	for _, name := range names {
		for _, n := range []named{{readWrite: []string{name}, readOnly: []string{"/r"}}, {readWrite: []string{"/w"}, readOnly: []string{name}}} {
			profile, params := macosHolding(n)
			if profile != plain {
				t.Errorf("%q changed the profile to %q", name, profile)
			}
			if !slices.Contains(params, "RW0="+name) && !slices.Contains(params, "RO0="+name) {
				t.Errorf("%q is not whole in its parameter: %q", name, params)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no rows ran")
	}
}
