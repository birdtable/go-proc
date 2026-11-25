package gitenv_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// checkerNames is the seventeen names a repository checker dropped when the package was made, which
// names must cover: written out here so a name taken out of names is red whatever the list says.
func checkernames() []string {
	return []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_COMMON_DIR",
		"GIT_INDEX_FILE", "GIT_PREFIX", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
		"GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE",
	}
}

// has is whether environ holds a variable named name, as written.
func has(environ []string, name string) bool {
	return slices.ContainsFunc(environ, func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return k == name
	})
}

// valueOf is the last value environ gives name, which is the one a child sees, and whether it gives one.
func valueOf(environ []string, name string) (string, bool) {
	var value string
	found := false
	for _, kv := range environ {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			value, found = v, true
		}
	}
	return value, found
}

// names covers every name the checker drops, has no name twice and holds only git's own, and Env drops
// each one when it is set, whatever its case, so a name in names that survives Env is red.
//
// What it costs when this fails: a project's own list, held to names, is held to less than the checker
// held itself to, and a variable that points git at another repository reaches a child.
func TestTheNamesCoverEveryNameTheCheckerDropsAndEnvDropsEachOfThem(t *testing.T) {
	t.Parallel()
	names := gitenv.Names()
	covered := 0
	for _, want := range checkernames() {
		covered++
		if !slices.Contains(names, want) {
			t.Errorf("names does not hold %s, which the checker drops", want)
		}
	}
	if covered != 17 || len(checkernames()) != 17 {
		t.Fatalf("checked %d of the 17 names the checker drops", covered)
	}
	seen := map[string]bool{}
	dropped := 0
	for _, name := range names {
		if seen[name] || !strings.HasPrefix(name, "GIT_") {
			t.Errorf("%s is listed twice or is no name of git's", name)
		}
		seen[name] = true
		for _, spelled := range []string{name, strings.ToLower(name)} {
			if out := gitenv.Env([]string{spelled + "=/elsewhere", "PATH=/bin"}); has(out, spelled) {
				t.Errorf("%s survives Env: %q", spelled, out)
			}
		}
		dropped++
	}
	if dropped != len(names) || dropped < len(checkernames()) {
		t.Fatalf("held %d of %d names", dropped, len(names))
	}
}

// Env drops every name that starts GIT_ (the numbered config names and the global and system
// config among them), the variables that name the person's home, and NETRC, in any case, and keeps
// the rest; it sets GIT_CONFIG_NOSYSTEM=1 and GIT_TERMINAL_PROMPT=0 over a caller's own; and the
// caller's add comes last, so a home of its own is the one a child sees.
//
// What it costs when this fails: a numbered config name or the person's own global configuration
// reaches a git child, which then runs a program the caller never named, a NETRC of the caller's
// sends that file's password though the caller set a HOME of its own, or an environment a caller
// adds is overridden by the one it was meant to replace.
func TestEnvDropsEveryGitNameAndTheHomesAndKeepsTheRestAndSetsItsTwo(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/usr/bin", "LANG=C", "TMPDIR=/t", "XDG_CACHE_HOME=/c", "GOFLAGS=-mod=mod",
		"HOME=/home/p", "home=/lower", "USERPROFILE=C:\\p", "HOMEDRIVE=C:", "HOMEPATH=\\p", "XDG_CONFIG_HOME=/p/.config",
		"NETRC=/p/.netrc", "Netrc=/mixed/.netrc",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/hooks", "GIT_CONFIG_KEY_17=a", "GIT_CONFIG_VALUE_17=b",
		"GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_SYSTEM=/s", "git_dir=/lower", "Git_Work_Tree=/mixed", "GIT_SSH_COMMAND=ssh -o x",
		"GIT_TERMINAL_PROMPT=1", "GIT_CONFIG_NOSYSTEM=0", "=C:=C:\\",
	}
	out := gitenv.Env(in, "HOME=/own", "GIT_AUTHOR_NAME=a")
	kept := []string{"PATH=/usr/bin", "LANG=C", "TMPDIR=/t", "XDG_CACHE_HOME=/c", "GOFLAGS=-mod=mod", "=C:=C:\\"}
	checked := 0
	for _, kv := range kept {
		checked++
		if !slices.Contains(out, kv) {
			t.Errorf("Env dropped %q, which is none of git's", kv)
		}
	}
	for _, kv := range in {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(strings.ToUpper(k), "GIT_") && k != "GIT_TERMINAL_PROMPT" && k != "GIT_CONFIG_NOSYSTEM" {
			checked++
			if has(out, k) {
				t.Errorf("Env kept %s", k)
			}
		}
	}
	for _, k := range []string{"home", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "XDG_CONFIG_HOME"} {
		checked++
		if has(out, k) {
			t.Errorf("Env kept %s, which names the person's own configuration", k)
		}
	}
	for _, k := range []string{"NETRC", "Netrc"} {
		checked++
		if has(out, k) {
			t.Errorf("Env kept %s, which names a .netrc libcurl reads ahead of HOME's", k)
		}
	}
	for name, want := range map[string]string{"HOME": "/own", "GIT_AUTHOR_NAME": "a", "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"} {
		checked++
		if got, ok := valueOf(out, name); !ok || got != want {
			t.Errorf("a child sees %s=%q (set %v), want %q", name, got, ok, want)
		}
	}
	if last := out[len(out)-2:]; !slices.Equal(last, []string{"HOME=/own", "GIT_AUTHOR_NAME=a"}) {
		t.Errorf("Env ends on %q, and add comes last", last)
	}
	if checked < 27 {
		t.Fatalf("checked %d variables", checked)
	}
	if got := gitenv.Env(nil); !slices.Equal(got, []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}) {
		t.Errorf("Env(nil) = %q", got)
	}
	if got := gitenv.Env([]string{"PATH=/x"}); len(got) != 3 || got[0] != "PATH=/x" {
		t.Errorf("Env(PATH) = %q", got)
	}
}

// ReadArgs puts git in the folder, takes no optional lock, turns the file system monitor and the
// hooks off, and then the caller's arguments, whatever they hold.
//
// What it costs when this fails: a read of a folder takes the folder's own configuration at its word
// and runs the program it names, or leaves a lock a person's git then waits on.
func TestReadArgsPutGitInTheFolderAndTurnOffTheMonitorAndTheHooks(t *testing.T) {
	t.Parallel()
	prefix := []string{"-C", "/work", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}
	checked := 0
	for name, tc := range map[string]struct {
		dir  string
		args []string
	}{
		"a subcommand":                        {"/work", []string{"status", "--porcelain"}},
		"no arguments":                        {"/work", nil},
		"a subcommand that looks like a flag": {"/work", []string{"-c", "core.hooksPath=/hooks", "status"}},
	} {
		checked++
		want := slices.Concat(prefix, tc.args)
		if got := gitenv.ReadArgs(tc.dir, tc.args...); !slices.Equal(got, want) {
			t.Errorf("%s: ReadArgs = %q, want %q", name, got, want)
		}
	}
	if got := gitenv.ReadArgs("-x", "log"); got[0] != "-C" || got[1] != "-x" || got[2] != "--no-optional-locks" {
		t.Errorf("a folder that starts with a dash: %q, and -C takes it as its argument", got)
	}
	if checked != 3 {
		t.Fatalf("checked %d of 3 argument lists", checked)
	}
}

// foreign is each import of the Go source src that is not the standard library's, as the go command
// tells one: the first element of a standard library path holds no dot.
func foreign(src string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
			out = append(out, path)
		}
	}
	return out, nil
}

// ownTestkit and ownGitenv are the two imports that are not the standard library's that a test file
// of the package may make: testkit, the environment the tests start from, and the
// package itself, from package gitenv_test. testenv once imported gitenv,
// so the tests are in package gitenv_test; gitenv/gittest imports gitenv, so a test of package
// gitenv that imported it would be a cycle.
const (
	ownTestkit = "github.com/birdtable/go-testkit"
	ownGitenv  = "github.com/birdtable/go-proc/gitenv"
)

// allowed is whether the file name may import imp, which is not the standard library's: only a test
// file, and only ownTestkit or ownGitenv.
func allowed(name, imp string) bool {
	return (imp == ownTestkit || imp == ownGitenv) && strings.HasSuffix(name, "_test.go")
}

// The package imports the standard library and nothing else, so it imports no other package of the
// module: every Go file of the package is read, and the planted sources, one that imports another
// package and one that imports a library, are each red. A test file may import ownTestkit and
// ownGitenv alone, and the same imports from a file that is no test are red.
//
// What it costs when this fails: a block every project's git child takes pulls in another block or a
// library, and a project that took it for its few lines takes a dependency it never chose.
func TestTheModuleImportsTheStandardLibraryAndNoOtherBlock(t *testing.T) {
	t.Parallel()
	planted := 0
	for name, tc := range map[string]struct {
		src  string
		want int
	}{
		"the standard library":  {"package p\n\nimport (\n\t\"os\"\n\t\"net/http\"\n)\n", 0},
		"another package":       {"package p\n\nimport \"github.com/example/app/version\"\n", 1},
		"a library":             {"package p\n\nimport _ \"go.yaml.in/yaml/v3\"\n", 1},
		"a block and a library": {"package p\n\nimport (\n\t\"github.com/example/app/check\"\n\t\"golang.org/x/sys/unix\"\n)\n", 2},
	} {
		planted++
		got, err := foreign(tc.src)
		if err != nil || len(got) != tc.want {
			t.Errorf("%s: imports %q (%v), want %d that are not the standard library's", name, got, err, tc.want)
		}
	}
	if planted != 4 {
		t.Fatalf("read %d planted sources, want 4", planted)
	}
	if allowed("gitenv.go", ownTestkit) || allowed("gitenv.go", ownGitenv) || allowed("main_test.go", "github.com/example/app/version") || !allowed("main_test.go", ownTestkit) || !allowed("main_test.go", ownGitenv) {
		t.Error("the two imports a test file may make are not held to test files and to testkit and the module's gitenv")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read, foreigners := 0, 0
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		read++
		got, err := foreign(string(b))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, imp := range got {
			if allowed(name, imp) {
				continue
			}
			foreigners++
			t.Errorf("%s imports %s, and the block imports the standard library alone", name, imp)
		}
	}
	if read < 3 {
		t.Fatalf("read %d Go files of the block, so this proves little", read)
	}
	if foreigners != 0 {
		t.Fatalf("%d import(s) of the block are not the standard library's", foreigners)
	}
}
