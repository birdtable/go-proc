package proc

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/internal/homevars"
)

// SystemEnv on Windows is SystemRoot and ProgramData with this process's values and nothing else,
// none of PATH, LANG or the home names among it; elsewhere it is empty. It is never nil, and a new
// list at each call.
//
// What it costs when this fails: a caller that gives a child its own PATH, LANG and home and adds
// SystemEnv for the rest starts a Windows program with no SystemRoot, which may fail to load, or no
// ProgramData, without which System32's OpenSSH exits 255, or hands the child this process's PATH or
// home over the ones it chose.
func TestSystemEnvIsSystemRootAndProgramDataOnWindowsAndNothingElsewhere(t *testing.T) {
	t.Parallel()
	got := SystemEnv()
	if got == nil {
		t.Fatal("SystemEnv is nil, which exec reads as this process's whole environment")
	}
	var want []string
	if homevars.Here == "windows" {
		for _, name := range []string{"SystemRoot", "ProgramData"} {
			v, ok := os.LookupEnv(name)
			if !ok {
				t.Fatalf("this Windows test process has no %s to compare SystemEnv with", name)
			}
			want = append(want, name+"="+v)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("SystemEnv is %q, want %q", got, want)
	}
	notSystem := slices.Concat([]string{"PATH", "LANG"}, homevars.Folders(homevars.Here), homevars.Halves(homevars.Here))
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(notSystem, func(n string) bool { return envKey(n) == envKey(name) }) {
			t.Errorf("SystemEnv gives %q, which is PATH, LANG or a home name, not the system part", kv)
		}
	}
	if len(got) > 0 {
		got[0] = "CHANGED=1"
		if again := SystemEnv(); slices.Contains(again, "CHANGED=1") {
			t.Errorf("a change to what SystemEnv gave reached the next call: %q", again)
		}
	}
}

// Every name SystemEnv can give is one Needed names, and each entry it gives is one Bare gives, with
// the same spelling and value: the three read one list.
//
// What it costs when this fails: SystemEnv and Bare drift apart, so a caller that builds a child's
// environment from SystemEnv keeps a variable Bare does not, or misses one Bare keeps.
func TestEveryNameOfSystemEnvIsInNeededAndItsEntriesInBare(t *testing.T) {
	t.Parallel()
	needed, bare := Needed(), Bare()
	for _, name := range systemNames() {
		if !slices.Contains(needed, name) {
			t.Errorf("%s is a system name that Needed %q does not name", name, needed)
		}
	}
	for _, kv := range SystemEnv() {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(needed, name) {
			t.Errorf("SystemEnv gives %q, whose name Needed %q does not name", kv, needed)
		}
		if !slices.Contains(bare, kv) {
			t.Errorf("SystemEnv gives %q, which Bare %q does not", kv, bare)
		}
	}
}

// A child started with SystemRoot and ProgramData reads SystemEnv as the two with their values on
// Windows and as empty elsewhere, where the two it was given count for nothing; once the child
// unsets every system name, SystemEnv is empty on every system, and never nil.
//
// What it costs when this fails: SystemEnv gives a name that is not set, as SystemRoot= with no
// value, gives nil, which exec reads as the whole environment, or gives either name on Unix.
func TestSystemEnvWithTheSystemNamesUnsetIsEmpty(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "system-env")
	given := []string{`SystemRoot=C:\Windows`, `ProgramData=C:\ProgramData`}
	if homevars.Here == "windows" {
		given = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "ProgramData=" + os.Getenv("ProgramData")}
	} else {
		c.Env = append(c.Env, given...)
	}
	r, err := Run(t.Context(), c)
	if err != nil || r.Code != 0 {
		t.Fatalf("the child: %v, code %d, stdout %q, stderr %q", err, r.Code, r.Stdout, r.Stderr)
	}
	before, after, ok := strings.Cut(strings.TrimSuffix(string(r.Stdout), "\n"), "\n")
	if !ok {
		t.Fatalf("the child printed %q, want two lines", r.Stdout)
	}
	wantBefore := "list:"
	if homevars.Here == "windows" {
		wantBefore += strings.Join(given, "\x00") + "\x00"
	}
	if before != wantBefore {
		t.Errorf("with %q set, the child's SystemEnv is %q, want %q", given, before, wantBefore)
	}
	if after != "list:" {
		t.Errorf("with every system name unset, the child's SystemEnv is %q, want an empty list", after)
	}
}

// SystemEnv put first, as its doc says, joined to a caller's list that is nil, is a list, never nil,
// on every system.
//
// What it costs when this fails: a caller with no variables of its own, joining them after
// SystemEnv as the doc says, hands Run a nil Env, which Run reads as Bare and os/exec as this
// process's whole environment, where it meant a child with none of this process's PATH or home.
func TestSystemEnvFirstJoinedToANilListIsNotNil(t *testing.T) {
	t.Parallel()
	var own []string
	if env := append(SystemEnv(), own...); env == nil {
		t.Fatal("append(SystemEnv(), nil...) is nil, which Run reads as Bare and os/exec as the whole environment")
	}
}
