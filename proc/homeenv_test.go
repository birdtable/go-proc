package proc

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/internal/homevars"
)

// Needed is Bare's own list on this system, as a new slice each call, so a caller that appends to
// or changes it changes neither Bare nor the next call's.
//
// What it costs when this fails: a caller that keeps a child's needed variables by Needed keeps a
// list Bare does not, and a Windows child started with it fails to load for want of SystemRoot.
func TestNeededIsBaresListAndTheCallersOwn(t *testing.T) {
	t.Parallel()
	got := Needed()
	if !slices.Equal(got, bareNames()) || len(got) == 0 {
		t.Fatalf("Needed is %v, want Bare's %v", got, bareNames())
	}
	got[0] = "CHANGED"
	if again := Needed(); again[0] == "CHANGED" {
		t.Errorf("a change to what Needed gave reached the next call: %v", again)
	}
	if homevars.Here == "windows" && !slices.Contains(got, "SystemRoot") {
		t.Errorf("Needed on Windows is %v, with no SystemRoot", got)
	}
}

// HomeEnv gives every name of homevars' list for this system, each its own folder beneath root
// named for it in lower case, and on Windows HOMEDRIVE and HOMEPATH that name USERPROFILE's.
//
// What it costs when this fails: a child given HomeEnv still reads one of the person's folders by a
// name the list leaves out, or two names share one folder.
func TestHomeEnvNamesEachFolderBeneathRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	env := HomeEnv(root)
	got := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || got[k] != "" {
			t.Fatalf("HomeEnv gives %q, not one NAME=value of a name given once", kv)
		}
		got[k] = v
	}
	names := homevars.Folders(homevars.Here)
	if len(env) != len(names)+len(homevars.Halves(homevars.Here)) {
		t.Errorf("HomeEnv gives %d entries, want %d folders and the halves", len(env), len(names))
	}
	for _, k := range names {
		if want := filepath.Join(root, strings.ToLower(k)); got[k] != want {
			t.Errorf("%s is %q, want %q", k, got[k], want)
		}
	}
	if homevars.Here == "windows" && got["HOMEDRIVE"]+got["HOMEPATH"] != got["USERPROFILE"] {
		t.Errorf("HOMEDRIVE %q and HOMEPATH %q do not name USERPROFILE %q", got["HOMEDRIVE"], got["HOMEPATH"], got["USERPROFILE"])
	}
}

// The tests run under testkit.Main, which points each variable of testkit's own home list at a
// folder beneath one it made, as homevars.Under does. So the list go-proc keeps for HomeEnv and the
// one testkit keeps for its isolate are equal when each name of homevars, the halves included, has
// the value Under gives it beneath that folder, and no other variable but git's global
// configuration names a path beneath it. A half testkit set that homevars
// lacks is not seen, since its value is no path beneath the folder; the halves are fixed by
// homevars' own row.
//
// What it costs when this fails: the two lists part, and a child given HomeEnv reads a folder of the
// person's by a name the tests' isolate covers, or the tests read one that HomeEnv covers, and the
// isolate's proof of HomeEnv no longer holds.
func TestHomevarsListsTheVariablesTestkitIsolates(t *testing.T) {
	t.Parallel()
	own := filepath.Dir(os.Getenv("HOME"))
	if !strings.HasPrefix(filepath.Base(own), "testkit-") {
		t.Fatalf("HOME is %q, not beneath a folder testkit.Main made: TestMain does not run the tests under testkit.Main", os.Getenv("HOME"))
	}
	want := map[string]string{}
	for _, kv := range homevars.Under(own) {
		k, v, _ := strings.Cut(kv, "=")
		want[k] = v
		if got := os.Getenv(k); got != v {
			t.Errorf("homevars lists %s, and testkit set it to %q, want %q", k, got, v)
		}
	}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		k = strings.ToUpper(k)
		rel, err := filepath.Rel(own, v)
		if _, listed := want[k]; listed || k == "GIT_CONFIG_GLOBAL" || !filepath.IsAbs(v) || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		t.Errorf("testkit set %s to %q beneath its folder, and homevars does not list it", k, v)
	}
}

// A child run with Bare's environment and HomeEnv's after it reads its home, configuration, cache
// and temp folders beneath root, not this process's, which Bare's HOME (or USERPROFILE) names.
//
// What it costs when this fails: a child pointed at a folder of the caller's reads or writes the
// person's own home, settings or cache, because a name of Bare's wins over HomeEnv's.
func TestAChildGivenHomeEnvReadsItsFoldersBeneathRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, kv := range HomeEnv(root) {
		k, v, _ := strings.Cut(kv, "=")
		if slices.Contains(homevars.Halves(homevars.Here), k) {
			continue
		}
		if err := os.MkdirAll(v, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c := helperCmd(t, "folders")
	c.Env = append(c.Env, HomeEnv(root)...)
	r, err := Run(context.Background(), c)
	if err != nil || r.Code != 0 {
		t.Fatalf("the child: %v, code %d, stdout %q, stderr %q", err, r.Code, r.Stdout, r.Stderr)
	}
	read := strings.Split(strings.TrimSuffix(string(r.Stdout), "\x00"), "\x00")
	if len(read) != 4 {
		t.Fatalf("the child printed %q, want four folders", r.Stdout)
	}
	for i, what := range []string{"home", "configuration folder", "cache folder", "temp folder"} {
		if rel, err := filepath.Rel(root, read[i]); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("the child's %s is %s, not beneath %s", what, read[i], root)
		}
	}
}
