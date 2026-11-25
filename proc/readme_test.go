package proc

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readmeExample is the README's one Go block: what example_test.go holds after its package line.
func readmeExample(readme, example []byte) (block, want []byte, ok bool) {
	_, want, ok = bytes.Cut(example, []byte("package proc_test\n"))
	if !ok {
		return nil, nil, false
	}
	_, rest, ok := bytes.Cut(readme, []byte("\n```go\n"))
	if !ok {
		return nil, nil, false
	}
	block, _, ok = bytes.Cut(rest, []byte("\n```\n"))
	return bytes.TrimSpace(block), bytes.TrimSpace(want), ok
}

// TestTheREADMEsExampleIsExampleRunAsItCompiles: the README's Go block is example_test.go after its
// package line, byte for byte, so the example a reader copies is the one the suite compiles and
// runs. The control: the example with one line changed does not match.
//
// What it costs when this fails: a reader copies an example that no longer compiles, or no longer
// does what the README says it does.
func TestTheREADMEsExampleIsExampleRunAsItCompiles(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	block, want, ok := readmeExample(readme, example)
	if !ok {
		t.Fatal("the README has no ```go block, or example_test.go no package proc_test line")
	}
	if !bytes.Equal(block, want) {
		t.Errorf("the README's Go block differs from example_test.go after its package line:\n--- README\n%s\n--- example_test.go\n%s", block, want)
	}
	changed := bytes.Replace(example, []byte(`"version"`), []byte(`"env"`), 1)
	if bytes.Equal(changed, example) {
		t.Fatal(`the control found no "version" in example_test.go to change`)
	}
	if block, want, _ := readmeExample(readme, changed); bytes.Equal(block, want) {
		t.Error("the README's Go block matches an example with one line changed")
	}
}

// readmeRefs matches what an outside reader of a README cannot follow: an issue or pull request
// number (a # and a digit), a decision id (two to four capitals, a dash and a number, so Apache-2.0
// is not one), and each of words, whole and in any case. The words are an argument, so this file
// names none of what the check is used to keep out.
func readmeRefs(words ...string) *regexp.Regexp {
	pattern := `#[0-9]|\b[A-Z]{2,4}-[0-9]`
	if len(words) > 0 {
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = regexp.QuoteMeta(w)
		}
		pattern += `|(?i:\b(?:` + strings.Join(quoted, "|") + `)\b)`
	}
	return regexp.MustCompile(pattern)
}

// readmes is every README.md of the repository by its path from the repository's root, the root's
// included, found by a walk of root that skips every folder whose name starts with a dot.
func readmes(t *testing.T, root string) map[string][]byte {
	t.Helper()
	found := map[string][]byte{}
	err := fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && p != "." && strings.HasPrefix(d.Name(), "."):
			return fs.SkipDir
		case !d.IsDir() && d.Name() == "README.md":
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				return err
			}
			found[p] = b
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// TestTheREADMENamesNoTicketOrDecision: no README.md of the repository, the root's or a package's,
// names an issue, pull request or decision number, or a word of the list readmeRefs is given, here
// two made-up words, in any case; the walk finds the root's README and gitenv/gittest's, two folders
// down; the planted lines with one are each found, and a plain line, which holds a listed word only
// inside a longer one, is not.
//
// What it costs when this fails: an outside reader meets a reference to a tracker or a ruling they
// cannot open, in place of what the library does, or a listed word the check was given to keep out.
func TestTheREADMENamesNoTicketOrDecision(t *testing.T) {
	t.Parallel()
	// The list is two made-up words, so this file names none the check is used to keep out.
	refs := readmeRefs("zorvex", "quillon")
	all := readmes(t, "..")
	for _, must := range []string{"README.md", "gitenv/gittest/README.md"} {
		if _, ok := all[must]; !ok {
			t.Fatalf("the walk did not reach %s: it read %d READMEs", must, len(all))
		}
	}
	for p, readme := range all {
		if m := refs.FindAll(readme, -1); len(m) != 0 {
			t.Errorf("%s names %q", p, m)
		}
	}
	for _, line := range []string{"as widget#25 says", "ruled in XR-27", "under QB-5", "the zorvex's", "The Zorvex", "a QUILLON wrapper", "moved from Quillon"} {
		if !refs.MatchString(line) {
			t.Errorf("the planted line %q is not found", line)
		}
	}
	if refs.MatchString("a tree of 32 hex digits, under Apache-2.0, with a Zorvexed flag and quillons") {
		t.Error("a plain line is found")
	}
}

// securityHeading is the README's line that heads how to report a security problem, and
// securityLink the file that section points the reader to.
const (
	securityHeading = "\n## Security\n"
	securityLink    = "(SECURITY.md)"
)

// saysHowToReportSecurity is whether readme has the security heading as a whole line of its own,
// with a link to SECURITY.md after it.
func saysHowToReportSecurity(readme []byte) bool {
	_, after, ok := bytes.Cut(readme, []byte(securityHeading))
	return ok && bytes.Contains(after, []byte(securityLink))
}

// TestTheREADMESaysHowToReportASecurityProblem: the README has its Security heading as a line of
// its own, with a link to SECURITY.md below it, and a README with the heading taken out, turned
// into a third-level one, or without the link after it, is found to lack it.
//
// What it costs when this fails: a reader has no way to learn that a security problem is reported
// privately, and may post one with a working exploit in a public issue.
func TestTheREADMESaysHowToReportASecurityProblem(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !saysHowToReportSecurity(readme) {
		t.Errorf("README.md has no %q line followed by a link %s", securityHeading[1:len(securityHeading)-1], securityLink)
	}
	if _, err := os.Stat("../SECURITY.md"); err != nil {
		t.Errorf("the README links SECURITY.md, which is not there: %v", err)
	}
	for _, planted := range []string{
		"## License\n\nSee [SECURITY.md](SECURITY.md).\n",
		"### Security\n\nSee [SECURITY.md](SECURITY.md).\n",
		"## Security soon\n\nSee [SECURITY.md](SECURITY.md).\n",
		"## Security\n\nOpen an issue.\n",
		"See [SECURITY.md](SECURITY.md).\n\n## Security\n\nOpen an issue.\n",
	} {
		if saysHowToReportSecurity([]byte("# go-proc\n\n" + planted)) {
			t.Errorf("a README with only %q is found to say how to report a security problem", planted)
		}
	}
}
