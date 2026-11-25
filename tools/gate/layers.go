package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// plants is the folder of the planted packages, slash-separated from the top.
const plants = "gate/layers/testdata"

// norowPlant is the planted folder for layers-no-row, which names no row.
const norowPlant = "norow"

// wantMark leads the rule a planted import must be refused by, at the end of its line.
const wantMark = "// want "

// wants reads every .go file under root, a folder slash-separated from the top, for its want lines:
// "<file>:<line> <rule>", the file slash-separated from the top, sorted.
func wants(fsys fs.FS, root string) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, root, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".go") {
			return err
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		n := 0
		for line := range strings.Lines(string(b)) {
			n++
			if i := strings.LastIndex(line, wantMark); i >= 0 {
				out = append(out, fmt.Sprintf("%s:%d %s", name, n, strings.TrimSpace(line[i+len(wantMark):])))
			}
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

// folders is the folders the want lines are in, each once, sorted.
func folders(want []string) []string {
	var out []string
	for _, w := range want {
		file, _, _ := strings.Cut(w, ":")
		out = append(out, path.Dir(file))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rowLine is a row of layers.yaml: "  - path: <folder>".
var rowLine = regexp.MustCompile(`^\s*-\s+path:\s+(\S+)\s*$`)

// rows is the folders layers.yaml names, sorted.
func rows(yaml string) []string {
	var out []string
	for line := range strings.Lines(yaml) {
		if m := rowLine.FindStringSubmatch(strings.TrimRight(line, "\r\n")); m != nil {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

var (
	// codeRule is a layers-<path> rule of .golangci.yml, at its depth there.
	codeRule = regexp.MustCompile(`^        layers-(\S+):$`)
	// noRowFile is one of layers-no-row's folders, as it leaves it out.
	noRowFile = regexp.MustCompile(`^            - "!\*\*/([^"]+)/\*\.go"$`)
)

// rules is what .golangci.yml holds of the layer rows: the folders of its layers-<path> rules, of its
// layers-<path>-tests rules, and the folders layers-no-row leaves out, each sorted.
func rules(golangci string) (code, tests, norow []string) {
	for line := range strings.Lines(golangci) {
		line = strings.TrimRight(line, "\r\n")
		if m := codeRule.FindStringSubmatch(line); m != nil {
			switch name := m[1]; {
			case name == "no-row":
			case strings.HasSuffix(name, "-tests"):
				tests = append(tests, strings.TrimSuffix(name, "-tests"))
			default:
				code = append(code, name)
			}
		}
		if m := noRowFile.FindStringSubmatch(line); m != nil {
			norow = append(norow, m[1])
		}
	}
	slices.Sort(code)
	slices.Sort(tests)
	slices.Sort(norow)
	return code, tests, norow
}

// planted is the planted folders below plants that name a row, sorted: every folder but norow's.
func planted(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if rest, ok := strings.CutPrefix(d, plants+"/"); ok && rest != norowPlant {
			out = append(out, rest)
		}
	}
	slices.Sort(out)
	return out
}

// issueLine is one issue golangci-lint prints, its file and line first, and fromList the depguard
// rule it names.
var (
	issueLine = regexp.MustCompile(`^[^ :]+:[0-9]+:`)
	fromList  = regexp.MustCompile(`from list '([^']+)'`)
)

// issues is depguard's report as "<file>:<line> <rule>", the file slash-separated, sorted.
func issues(out string) []string {
	var got []string
	for line := range strings.Lines(out) {
		line = strings.ReplaceAll(strings.TrimRight(line, "\r\n"), `\`, "/")
		if !issueLine.MatchString(line) {
			continue
		}
		m := fromList.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		f := strings.SplitN(line, ":", 3)
		got = append(got, f[0]+":"+f[1]+" "+m[1])
	}
	slices.Sort(got)
	return got
}

// differ says, on stderr, that two lists that should be equal are not, and is errRed; it is nil when
// they are equal.
func differ(stderr io.Writer, what, wantName string, want []string, gotName string, got []string) error {
	if slices.Equal(want, got) {
		return nil
	}
	fmt.Fprintf(stderr, "layers: %s\n%s:\n", what, wantName)
	for _, l := range want {
		fmt.Fprintln(stderr, l)
	}
	fmt.Fprintf(stderr, "%s:\n", gotName)
	for _, l := range got {
		fmt.Fprintln(stderr, l)
	}
	return errRed
}

// layersStep holds the layer rows of .golangci.yml red. Each planted package under
// gate/layers/testdata is named a row of layers.yaml by its folder, or none (norow), and each of its
// imports that the row does not allow ends in "// want <rule>". depguard alone lints them, and the
// file, line and rule of each issue it gives must be those of the want lines, no more and no fewer.
// The rows of layers.yaml, the layers-<path> rules, the layers-<path>-tests rules, the folders
// layers-no-row leaves out and the planted folders must be the same folders, and each row a folder
// of Go files. Then depguard alone lints the module on linux and windows with the tags lint builds
// without, live and race, so a file built only with one of them is held to its row too.
func layersStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := noArgs("layers", args, stderr); err != nil {
		return err
	}
	bin, err := tool(ctx, "golangci-lint")
	if err != nil {
		return err
	}
	top := os.DirFS(".")
	want, err := wants(top, plants)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		fmt.Fprintf(stderr, "layers: no want line under %s, so nothing is proved red\n", plants)
		return errRed
	}
	dirs := folders(want)
	yaml, err := os.ReadFile("layers.yaml")
	if err != nil {
		return err
	}
	rs := rows(string(yaml))
	for _, r := range rs {
		if m, _ := filepath.Glob(filepath.Join(filepath.FromSlash(r), "*.go")); len(m) == 0 {
			fmt.Fprintf(stderr, "layers: the row %s of layers.yaml names no folder of Go files\n", r)
			return errRed
		}
	}
	golangci, err := os.ReadFile(".golangci.yml")
	if err != nil {
		return err
	}
	code, tests, norow := rules(string(golangci))
	for _, set := range []struct {
		name string
		got  []string
	}{
		{"layers-<path> rules", code},
		{"layers-<path>-tests rules", tests},
		{"folders layers-no-row leaves out", norow},
		{"planted folders under " + plants, planted(dirs)},
	} {
		if err := differ(stderr, "the "+set.name+" are not the rows of layers.yaml", "rows", rs, set.name, set.got); err != nil {
			return err
		}
	}
	lintArgs := []string{"run", "--allow-parallel-runners", "--enable-only", "depguard"}
	for _, d := range dirs {
		lintArgs = append(lintArgs, "./"+d)
	}
	out, _ := command(ctx, []string{"CGO_ENABLED=0"}, bin, lintArgs...).CombinedOutput()
	if err := differ(stderr, "depguard on the planted packages is not their want lines", "want", want, "got", issues(string(out))); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "layers: %d planted imports red, each by its rule\n", len(want))
	systems := []string{"linux", "windows"}
	return report(stdout, "layers", systems, together(ctx, bin, systems, "run", "--allow-parallel-runners", "--enable-only", "depguard", "--build-tags", "live,race", "./..."))
}
