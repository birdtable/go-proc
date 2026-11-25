package contract

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/birdtable/go-proc/gitenv"
	"github.com/birdtable/go-proc/gitenv/gittest"
	"github.com/birdtable/go-proc/proc"
)

// The gate of the contract index, one test for each thing it holds, so a red names which:
//
//   - the index is four fields a line, sorted by package and test, each pair once;
//   - every line's test is declared in its package;
//   - none is unbuilt or skipped on a system its line names;
//   - a line removed since the last tag is listed in api/breaks/<base tag>.contract.txt, and that
//     file lists nothing else.

// root is the module's root, two folders up from this package's.
const root = "../.."

// The index, and the folder of the break files, slash-separated from root.
const (
	contractFile = "api/contract.txt"
	breaksDir    = "api/breaks"
)

// systems is each value a line's systems field may name; all is the three.
func systems() []string { return []string{"darwin", "linux", "windows"} }

// target is one system and architecture.
type target struct{ goos, goarch string }

// release is each system the release tier runs on: a line that
// names a system is held on each pair of it. The release tier runs the tests with -race, so a file
// is read as built with the race tag and cgo on.
func release() []target {
	return []target{{"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}}
}

// line is one promise of the index.
type line struct {
	pkg, test, sentence string
	on                  []string // the systems the line names, sorted; all is the three
	raw                 string
}

// read is the file at name, slash-separated from root.
func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// records is text's lines, and why the text is not lines: a text that is not empty and does not
// end in a line feed, an empty line, or a carriage return.
func records(text string) (lines, bad []string) {
	if text == "" {
		return nil, nil
	}
	if !strings.HasSuffix(text, "\n") {
		bad = append(bad, "the file does not end in a line feed")
	}
	for i, r := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		switch {
		case r == "":
			bad = append(bad, fmt.Sprintf("line %d is empty", i+1))
		case strings.Contains(r, "\r"):
			bad = append(bad, fmt.Sprintf("line %d holds a carriage return: %q", i+1, r))
		default:
			lines = append(lines, r)
		}
	}
	return lines, bad
}

// fields is r split on tabs when it is n fields, none empty or padded.
func fields(r string, n int) ([]string, error) {
	f := strings.Split(r, "\t")
	if len(f) != n {
		return nil, fmt.Errorf("%d tab-separated fields, not %d: %q", len(f), n, r)
	}
	for _, v := range f {
		if v == "" || strings.TrimSpace(v) != v {
			return nil, fmt.Errorf("a field empty or padded with spaces: %q", r)
		}
	}
	return f, nil
}

// systemsOf is the systems s names, sorted: all, or one or more of systems joined by commas, each
// once.
func systemsOf(s string) ([]string, bool) {
	if s == "all" {
		return systems(), true
	}
	var on []string
	for v := range strings.SplitSeq(s, ",") {
		if !slices.Contains(systems(), v) || slices.Contains(on, v) {
			return nil, false
		}
		on = append(on, v)
	}
	slices.Sort(on)
	return on, true
}

// parse is the index text holds, its lines in their order, and why a line is not one: four fields,
// a folder of the module, a Test name, all or systems joined by commas, and the promise.
func parse(text string) ([]line, []string) {
	rs, bad := records(text)
	var ls []line
	for _, r := range rs {
		f, err := fields(r, 4)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		on, ok := systemsOf(f[2])
		switch {
		case path.IsAbs(f[0]) || path.Clean(f[0]) != f[0] || f[0] == "." || strings.HasPrefix(f[0], "../") || strings.Contains(f[0], `\`):
			bad = append(bad, fmt.Sprintf("the package is not a folder of the module, slash-separated: %q", r))
		case !strings.HasPrefix(f[1], "Test") || !token.IsIdentifier(f[1]):
			bad = append(bad, fmt.Sprintf("the test is not a Test name: %q", r))
		case !ok:
			bad = append(bad, fmt.Sprintf("the systems are not all, or darwin, linux and windows joined by commas, each once: %q", r))
		default:
			ls = append(ls, line{pkg: f[0], test: f[1], on: on, sentence: f[3], raw: r})
		}
	}
	return ls, bad
}

// unsorted is why ls is not sorted by package and then test, each pair once.
func unsorted(ls []line) []string {
	var bad []string
	for i := 1; i < len(ls); i++ {
		a, b := ls[i-1], ls[i]
		switch c := strings.Compare(a.pkg, b.pkg); {
		case c == 0 && a.test == b.test:
			bad = append(bad, fmt.Sprintf("%s %s has two lines; a test holds one promise", b.pkg, b.test))
		case c > 0 || c == 0 && a.test > b.test:
			bad = append(bad, fmt.Sprintf("%s %s comes after %s %s; the lines are sorted by package and then test", b.pkg, b.test, a.pkg, a.test))
		}
	}
	return bad
}

// index is the lines of api/contract.txt that parse. TestTheIndexIsFourFieldsALineSortedByPackageAndTestOnce
// is red for the rest.
func index(t *testing.T) []line {
	t.Helper()
	ls, _ := parse(read(t, contractFile))
	return ls
}

// held is what a package's test files say of one test on each release system.
type held struct {
	found bool                // a test file of the package declares it
	built map[string]bool     // goos/goarch: a file that declares it builds there
	skips map[string][]string // goos/goarch: each Skip in such a file's body, outside if testing.Short()
}

// buildFor is the build context of one release system.
func buildFor(goos, goarch string) *build.Context {
	c := build.Default
	c.GOOS, c.GOARCH, c.CgoEnabled = goos, goarch, true
	c.BuildTags = []string{"race"}
	return &c
}

// scan reads every test file of the package pkg for the top-level func test.
func scan(pkg, test string) (held, error) {
	dir := filepath.Join(root, filepath.FromSlash(pkg))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return held{}, err
	}
	h := held{built: map[string]bool{}, skips: map[string][]string{}}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			return held{}, err
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != test || fn.Body == nil {
				continue
			}
			h.found = true
			skips := unguardedSkips(fset, fn.Body)
			for _, s := range release() {
				builds, err := buildFor(s.goos, s.goarch).MatchFile(dir, e.Name())
				if err != nil {
					return held{}, err
				}
				if builds {
					key := s.goos + "/" + s.goarch
					h.built[key] = true
					h.skips[key] = append(h.skips[key], skips...)
				}
			}
		}
	}
	return h, nil
}

// unguardedSkips is where body calls a Skip, Skipf or SkipNow outside the body of an if whose
// condition is testing.Short() alone: that skip is the push tier's -short run's, and the release
// tier, which proves a tag, runs with no -short and fails on any skip. Any other condition is not
// read, so a skip under it counts on every system the file builds for. A skip in a helper the test
// calls is not seen here; the release tier's red on any skip holds that one.
func unguardedSkips(fset *token.FileSet, body *ast.BlockStmt) []string {
	var at []string
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if isShort(n.Cond) {
				if n.Init != nil {
					ast.Inspect(n.Init, visit)
				}
				if n.Else != nil {
					ast.Inspect(n.Else, visit)
				}
				return false
			}
		case *ast.CallExpr:
			if s, ok := n.Fun.(*ast.SelectorExpr); ok && slices.Contains([]string{"Skip", "Skipf", "SkipNow"}, s.Sel.Name) {
				p := fset.Position(n.Pos())
				rel, err := filepath.Rel(root, p.Filename)
				if err != nil {
					rel = p.Filename
				}
				at = append(at, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), p.Line))
			}
		}
		return true
	}
	ast.Inspect(body, visit)
	return at
}

// isShort is whether e is the call testing.Short().
func isShort(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok || len(c.Args) != 0 {
		return false
	}
	s, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && x.Name == "testing" && s.Sel.Name == "Short"
}

// git runs the host's git in the module's folder, reading it as gitenv.ReadArgs has a folder read,
// with no hook or monitor run, under gittest's environment, with no configuration of the person's.
// It gives what git printed to stdout, and whether it exited 0.
func git(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	dir, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := proc.Run(t.Context(), proc.Cmd{
		Path:      gittest.GitPath(t),
		Args:      gitenv.ReadArgs(dir, args...),
		Dir:       dir,
		Env:       gittest.Git(t.TempDir()),
		Timeout:   time.Minute,
		MaxOutput: 16 << 20,
	})
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(r.Stdout), r.Code == 0
}

// lastTag is the base tag, found as the API gate finds it (git describe --tags --abbrev=0 --match
// 'v*'), or why there is none: the module's folder is no repository, as in a module cache, or no tag
// is reachable. A shallow clone fails t, as its last tag may be one it never fetched.
func lastTag(t *testing.T) (tag, none string) {
	t.Helper()
	shallow, ok := git(t, "rev-parse", "--is-shallow-repository")
	if !ok {
		return "", "the module's folder is no git repository"
	}
	if strings.TrimSpace(shallow) != "false" {
		t.Fatal("this clone is shallow, so its last tag is not known; fetch the whole history")
	}
	out, ok := git(t, "describe", "--tags", "--abbrev=0", "--match", "v*")
	if !ok {
		return "", "no tag is reachable"
	}
	return strings.TrimSpace(out), ""
}

// atTag is the file name, slash-separated from root, as tag holds it, and whether tag holds it.
func atTag(t *testing.T, tag, name string) (string, bool) {
	t.Helper()
	listed, ok := git(t, "ls-tree", "--name-only", tag, "--", name)
	if !ok {
		t.Fatalf("git ls-tree %s -- %s failed", tag, name)
	}
	if strings.TrimSpace(listed) == "" {
		return "", false
	}
	text, ok := git(t, "show", tag+":"+name)
	if !ok {
		t.Fatalf("git show %s:%s failed", tag, name)
	}
	return text, true
}

// What it costs when this fails: a product that reads the index to cite a promise reads a line it
// cannot split, or two lines for one test, and the removed-line check compares lines it cannot
// tell apart.
func TestTheIndexIsFourFieldsALineSortedByPackageAndTestOnce(t *testing.T) {
	t.Parallel()
	ls, bad := parse(read(t, contractFile))
	bad = append(bad, unsorted(ls)...)
	for _, b := range bad {
		t.Errorf("%s: %s", contractFile, b)
	}
	if len(ls) == 0 {
		t.Errorf("%s holds no promise", contractFile)
	}
}

// What it costs when this fails: a line cites a test that is not there, renamed or gone, and a
// product relies on a promise nothing runs.
func TestEachLineNamesATestOfItsPackage(t *testing.T) {
	t.Parallel()
	for _, l := range index(t) {
		h, err := scan(l.pkg, l.test)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			t.Errorf("%s: %s %s: %s is no folder of the module", contractFile, l.pkg, l.test, l.pkg)
		case err != nil:
			t.Errorf("%s: %s %s: %v", contractFile, l.pkg, l.test, err)
		case !h.found:
			t.Errorf("%s: %s %s: no test file of %s declares func %s", contractFile, l.pkg, l.test, l.pkg, l.test)
		}
	}
}

// What it costs when this fails: a line says a promise holds on a system where its test is not
// built, or skips, so a tag's run there proves nothing of it, and a product releases for that system
// on a promise no run held.
func TestNoTestALineNamesIsUnbuiltOrSkippedOnASystemItNames(t *testing.T) {
	t.Parallel()
	for _, l := range index(t) {
		h, err := scan(l.pkg, l.test)
		if err != nil || !h.found {
			continue // TestEachLineNamesATestOfItsPackage is red for it
		}
		for _, s := range release() {
			key := s.goos + "/" + s.goarch
			switch {
			case !slices.Contains(l.on, s.goos):
			case !h.built[key]:
				t.Errorf("%s: %s %s is in no test file built for %s, which its line names", contractFile, l.pkg, l.test, key)
			case len(h.skips[key]) > 0:
				t.Errorf("%s: %s %s skips outside if testing.Short() on %s, which its line names, at %s", contractFile, l.pkg, l.test, key, strings.Join(h.skips[key], ", "))
			}
		}
	}
}

// kept is whether a line of the index at the last tag is still a promise: a line of the index with
// its package, test and sentence, naming every system it named. A line that only gains a system
// takes nothing away.
func kept(old line, now []line) bool {
	return slices.ContainsFunc(now, func(n line) bool {
		return n.pkg == old.pkg && n.test == old.test && n.sentence == old.sentence &&
			!slices.ContainsFunc(old.on, func(s string) bool { return !slices.Contains(n.on, s) })
	})
}

// What it costs when this fails: a tag drops or changes a promise and no record says so, and a
// product that cited the line finds it gone at its next pin bump, with nothing in the changelog's
// sources to say it went.
func TestALineRemovedSinceTheLastTagIsInItsBreakFile(t *testing.T) {
	t.Parallel()
	tag, none := lastTag(t)
	if none != "" {
		t.Logf("%s, so no removed line is held; every other check of the index is made", none)
		return
	}
	text, ok := atTag(t, tag, contractFile)
	if !ok {
		t.Logf("%s holds no %s, so no removed line is held; every other check of the index is made", tag, contractFile)
		return
	}
	old, bad := parse(text)
	for _, b := range bad {
		t.Errorf("%s as %s holds it: %s", contractFile, tag, b)
	}
	now := index(t)
	var gone []string
	for _, o := range old {
		if !kept(o, now) {
			gone = append(gone, o.raw)
		}
	}
	name := breaksDir + "/" + tag + ".contract.txt"
	listedText, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	listed, bad := parse(string(listedText))
	bad = append(bad, unsorted(listed)...)
	for _, b := range bad {
		t.Errorf("%s: %s", name, b)
	}
	for _, g := range gone {
		if !slices.ContainsFunc(listed, func(l line) bool { return l.raw == g }) {
			t.Errorf("%q was in %s at %s and is no promise now; a commit that removes or changes a promise lists its old line in %s", g, contractFile, tag, name)
		}
	}
	for _, l := range listed {
		if !slices.Contains(gone, l.raw) {
			t.Errorf("%s lists %q, which was not in %s at %s or is a promise still", name, l.raw, contractFile, tag)
		}
	}
}
