package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCheckOnLinuxAsksTheKernelAndSetsNothing: Check's ask of Linux takes the
// architecture's filter and Landlock's ABI and errata, and with folders
// /proc/self/fd and the temporary folder, and nothing else. A kernel below
// ABI 6, one at ABI 6 or later without erratum 2, one with no Landlock, an
// architecture with no filter, and with folders no /proc/self/fd or a
// temporary folder beneath one of them, are each a no that matches
// ErrNoBlock and errors.ErrUnsupported, naming what is missing; a kernel
// that gives the block is nil. In every row the only calls made are the asks.
//
// What it costs when this fails: a caller asks first and is told yes where the run will be refused, or
// told no where it would hold, or the ask itself sets a step of the block on the caller's process.
func TestCheckOnLinuxAsksTheKernelAndSetsNothing(t *testing.T) {
	t.Parallel()
	folders := named{readWrite: []string{"/work"}, readOnly: []string{"/usr/share"}}
	asked := []string{"landlockABI"}
	askedFolders := []string{"landlockABI", "procFDs", "tempFolder"}
	rows := []struct {
		claim string
		k     *fakeKernel
		arch  string
		n     named
		says  string   // "" is a block the run would set
		calls []string // the asks made
	}{
		{"ABI 6 with erratum 2", &fakeKernel{abi: 6}, "amd64", named{}, "", asked},
		{"ABI 9 with erratum 2", &fakeKernel{abi: 9}, "arm64", named{}, "", asked},
		{"ABI 5", &fakeKernel{abi: 5}, "amd64", named{}, "ABI 5", asked},
		{"ABI 1", &fakeKernel{abi: 1}, "arm64", named{}, "ABI 1", asked},
		{"ABI 6 without erratum 2", &fakeKernel{abi: 6, noErratum: true}, "amd64", named{}, "ABI 6 without erratum 2, so go-landlock takes it for ABI 5", asked},
		{"ABI 7 without erratum 2", &fakeKernel{abi: 7, noErratum: true}, "arm64", named{}, "ABI 7 without erratum 2, so go-landlock takes it for ABI 5", asked},
		{"no landlock", &fakeKernel{fail: "landlockABI", failWith: syscall.ENOSYS}, "amd64", named{}, "landlock is not here", asked},
		{"an architecture with no filter", &fakeKernel{abi: 9}, "riscv64", named{}, "no seccomp filter for the architecture", nil},
		{"folders, with /proc and a temporary folder outside them", &fakeKernel{abi: 6}, "amd64", folders, "", askedFolders},
		{"folders, with no /proc", &fakeKernel{abi: 6, fail: "procFDs", failWith: syscall.ENOENT}, "amd64", folders, "/proc/self/fd is not there", []string{"landlockABI", "procFDs"}},
		{"no /proc, and no folders", &fakeKernel{abi: 6, fail: "procFDs", failWith: syscall.ENOENT}, "amd64", named{}, "", asked},
		{"folders, with the temporary folder beneath a read-write one", &fakeKernel{abi: 9, temp: "/work/tmp"}, "arm64", folders, `the temporary folder "/work/tmp" is beneath "/work"`, askedFolders},
		{"folders, with the temporary folder a read-only one", &fakeKernel{abi: 9, temp: "/usr/share"}, "arm64", folders, `the temporary folder "/usr/share" is beneath "/usr/share"`, askedFolders},
		{"folders, with the temporary folder not found", &fakeKernel{abi: 6, fail: "tempFolder", failWith: syscall.ENOENT}, "amd64", folders, "the temporary folder, where the probe's folder is made, cannot be found", askedFolders},
		{"folders, and ABI 5", &fakeKernel{abi: 5}, "amd64", folders, "ABI 5", asked},
	}
	for _, r := range rows {
		err := checkLinux(r.k, r.arch, r.n)
		switch {
		case r.says == "" && err != nil:
			t.Errorf("%s: %v, want nil", r.claim, err)
		case r.says != "" && (err == nil || !strings.Contains(err.Error(), r.says)):
			t.Errorf("%s: %v, want a no saying %q", r.claim, err, r.says)
		case r.says != "":
			refusedAsNoBlock(t, r.claim, err, r.k.failWith)
		}
		if !slices.Equal(r.k.calls, r.calls) {
			t.Errorf("%s: Check made the calls %q, want %q and nothing set", r.claim, r.k.calls, r.calls)
		}
	}
	if len(rows) < 15 {
		t.Fatalf("ran %d rows", len(rows))
	}
}

// TestCheckNamesAFolderRunHoldingWouldRefuse: Check with folders checks each
// as RunHolding does, before it asks the system: a folder that does not
// exist is ErrUsage naming it, and so is a file, whatever its name holds; with no folders it is Run's
// check, takes no folder and does not read the program. None of them matches ErrNoBlock.
//
// What it costs when this fails: a caller that asks first with a folder that cannot be held is told yes,
// and learns of it only when the run is refused, after the work it started.
func TestCheckNamesAFolderRunHoldingWouldRefuse(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw")
	file := filepath.Join(dir, "file")
	// A name the error must quote on every system, as %q quotes each
	// backslash of a Windows path: a no-break space, which every system
	// takes in a name and strconv does not count printable, so %q writes it
	// \u00a0.
	odd := filepath.Join(dir, "fi\u00a0le")
	for _, f := range []string{file, odd} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gone := filepath.Join(dir, "gone")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		claim string
		f     Folders
		names string
	}{
		{"a folder that does not exist", Folders{ReadWrite: []string{gone}}, gone},
		{"a read-only folder that does not exist", Folders{ReadWrite: []string{filepath.Join(dir, "rw")}, ReadOnly: []string{gone}}, gone},
		{"a file", Folders{ReadOnly: []string{file}}, file},
		{"a file whose name is quoted", Folders{ReadOnly: []string{odd}}, odd},
	} {
		b, err := Check(r.f, self)
		// The error names the folder as %q writes it, which doubles each
		// backslash of a Windows path and escapes a quote.
		if b != (Block{}) {
			t.Errorf("%s: Block %+v with the refusal, want nothing closed", r.claim, b)
		}
		if !errors.Is(err, ErrUsage) {
			t.Errorf("%s: %v, want ErrUsage", r.claim, err)
		}
		if err == nil || !strings.Contains(err.Error(), strconv.Quote(r.names)) {
			t.Errorf("%s: %v, want the folder named, %s", r.claim, err, strconv.Quote(r.names))
		}
		if errors.Is(err, ErrNoBlock) {
			t.Errorf("%s: %v matches ErrNoBlock, but the folder is what was refused", r.claim, err)
		}
	}
	if _, err := Check(Folders{}, "not/read"); errors.Is(err, ErrUsage) {
		t.Errorf("Check with no folders is Run's, and names no folder and reads no program: %v", err)
	}
}

// stated is a FileInfo of a mode, for checkMac's fake stat.
type stated struct{ mode fs.FileMode }

func (s stated) Name() string       { return "sandbox-exec" }
func (s stated) Size() int64        { return 0 }
func (s stated) Mode() fs.FileMode  { return s.mode }
func (s stated) ModTime() time.Time { return time.Time{} }
func (s stated) IsDir() bool        { return s.mode.IsDir() }
func (s stated) Sys() any           { return nil }

// TestCheckOnMacAsksForTheSystemSandbox: Check's ask of macOS is the system
// sandbox's program, by its path: one that runs is a yes, and one that is
// not there, a folder, or a file that does not run is a no that matches
// ErrNoBlock and names the program.
//
// What it costs when this fails: on a Mac whose system sandbox is gone, a caller asks first, is told yes,
// and its run is refused after the work it started.
func TestCheckOnMacAsksForTheSystemSandbox(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		claim string
		stat  func(string) (os.FileInfo, error)
		yes   bool
	}{
		{"a program", func(string) (os.FileInfo, error) { return stated{0o755}, nil }, true},
		{"not there", func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }, false},
		{"a folder", func(string) (os.FileInfo, error) { return stated{fs.ModeDir | 0o755}, nil }, false},
		{"a file that does not run", func(string) (os.FileInfo, error) { return stated{0o644}, nil }, false},
	} {
		var asked string
		err := checkMac(func(p string) (os.FileInfo, error) { asked = p; return r.stat(p) })
		if asked != sandboxProgram {
			t.Errorf("%s: asked for %q, want %q", r.claim, asked, sandboxProgram)
		}
		switch {
		case r.yes && err != nil:
			t.Errorf("%s: %v, want nil", r.claim, err)
		case !r.yes && (err == nil || !strings.Contains(err.Error(), sandboxProgram)):
			t.Errorf("%s: %v, want a no naming %s", r.claim, err, sandboxProgram)
		case !r.yes:
			refusedAsNoBlock(t, r.claim, err, nil)
		}
	}
}

// TestTheLinuxBlockComparesFoldersAsSlashPathsOnEveryHost: beneathSlash
// takes Linux paths on every host, by path.Clean and "/": a backslash is a
// letter of a name there, not a separator, and a path is cleaned before it is
// compared. checkLinux's temporary folder and probeOutside's probe folder
// answer each row as beneathSlash does, so putting the host's beneath back in
// either goes red here: on Linux and macOS by the rows that clean, and on
// Windows by the rows with a backslash too.
//
// What it costs when this fails: the Linux block's checks compare its folders by the host's rules, so a
// probe folder or a temporary folder in a granted folder is not refused, and the run sets a ruleset over a
// probe it cannot refuse.
func TestTheLinuxBlockComparesFoldersAsSlashPathsOnEveryHost(t *testing.T) {
	t.Parallel()
	rows := []struct {
		p, dir string
		want   bool
	}{
		{"/work/tmp", "/work", true},
		{"/work", "/work", true},
		{"/anything/at/all", "/", true},
		{"/work/tmp", "/work/", true},
		{"/workshop", "/work", false},
		{"/usr/tmp", "/work", false},
		{"/work/../etc", "/work", false},
		{"/work/./tmp", "/work", true},
		{`/work\tmp`, "/work", false},
		{`\work\tmp`, `\work`, false},
		{`C:\work\tmp\netblock-1`, `C:\work`, false},
	}
	ran := 0
	for _, r := range rows {
		ran++
		if got := beneathSlash(r.p, r.dir); got != r.want {
			t.Errorf("beneathSlash(%q, %q) = %v, want %v", r.p, r.dir, got, r.want)
		}
		n := named{readWrite: []string{r.dir}}
		if err := checkLinux(&fakeKernel{abi: 6, temp: r.p}, "amd64", n); errors.Is(err, errProbeInFolder) != r.want {
			t.Errorf("checkLinux with the temporary folder %q and the folder %q: %v, want refused %v", r.p, r.dir, err, r.want)
		}
		if err := probeOutside(held{dir: r.p}, n); errors.Is(err, errProbeInFolder) != r.want {
			t.Errorf("probeOutside with the probe's folder %q and the folder %q: %v, want refused %v", r.p, r.dir, err, r.want)
		}
	}
	if ran != len(rows) || ran < 11 {
		t.Fatalf("ran %d of %d rows", ran, len(rows))
	}
}

// linuxGives is the Block Linux's block is, as the README's table says it: all five closed.
func linuxGives() Block {
	return Block{Network: true, Daemons: true, Terminal: true, Signals: true, ServiceManager: true}
}

// macGives is the Block macOS's block is, as the README's table says it: the profile leaves a
// signal to a process outside open, and launchd, which a process reaches by its bootstrap port.
func macGives() Block { return Block{Network: true, Daemons: true, Terminal: true} }

// TestEachSystemsBlockSaysWhatItClosesAndWhatItLeavesOpen: the Block Check returns with a yes is,
// on Linux, every field true; on macOS, Signals and ServiceManager false and the rest true; and on
// a system with no block, nothing closed. Check returns it only with nil: its refusals are each the
// zero Block.
//
// What it costs when this fails: a caller asks first and is told macOS closes the signals of processes
// outside, or launchd, and runs there a program it would hold only behind a block that does.
func TestEachSystemsBlockSaysWhatItClosesAndWhatItLeavesOpen(t *testing.T) {
	t.Parallel()
	ran := 0
	for goos, want := range map[string]Block{"linux": linuxGives(), "darwin": macGives(), "windows": {}, "freebsd": {}} {
		ran++
		if got := blockOn(goos); got != want {
			t.Errorf("%s: %+v, want %+v", goos, got, want)
		}
	}
	if !macGives().Network || macGives().Signals || macGives().ServiceManager {
		t.Errorf("macOS's Block %+v, want the network closed and signals and launchd open", macGives())
	}
	if ran < 4 {
		t.Fatalf("ran %d systems", ran)
	}
}
