package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// What it costs when this fails: a step given a word it does not know, or arguments it does not
// take, passes and checks nothing.
func TestRunRefusesAStepOrArgumentsItDoesNotKnow(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"nope"}, {"lint", "x"}, {"layers", "x"}, {"test-short", "x"}, {"notice-check", "x"}, {"psx"}, {"psx", "a", "b"}, {"namespace"}, {"namespace", "-nope", "./proc/"}} {
		var out, errs bytes.Buffer
		if code := run(context.Background(), args, &out, &errs); code != 2 {
			t.Errorf("run(%q) exited %d, and want 2: %s%s", args, code, &out, &errs)
		}
	}
}

// What it costs when this fails: a test that skips without saying it runs in the release tier
// passes the push tier, or one that says so fails it.
func TestSkippedHoldsASkipToItsOwnReleaseTierLine(t *testing.T) {
	t.Parallel()
	json := strings.Join([]string{
		`{"Action":"output","Package":"p","Test":"TestSaid","Output":"    a_test.go:9: release tier: needs a namespace\n"}`,
		`{"Action":"skip","Package":"p","Test":"TestSaid"}`,
		`{"Action":"output","Package":"p","Test":"TestQuiet","Output":"    a_test.go:12: no tool\n"}`,
		`{"Action":"skip","Package":"p","Test":"TestQuiet"}`,
		`{"Action":"output","Package":"q","Test":"TestSaid","Output":"    q_test.go:3: other\n"}`,
		`{"Action":"skip","Package":"q","Test":"TestSaid"}`,
		`{"Action":"output","Package":"p","Test":"TestPass","Output":"    a_test.go:4: release tier: said, did not skip\n"}`,
		`{"Action":"pass","Package":"p","Test":"TestPass"}`,
		`{"Action":"skip","Package":"p"}`,
		``,
	}, "\n")
	evs, err := events(strings.NewReader(json))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := keys(skipped(evs, true)), []string{"p TestQuiet", "q TestSaid"}; !slices.Equal(got, want) {
		t.Errorf("unmarked skips are %q, and want %q", got, want)
	}
	if got, want := keys(skipped(evs, false)), []string{"p TestQuiet", "p TestSaid", "q TestSaid"}; !slices.Equal(got, want) {
		t.Errorf("all skips are %q, and want %q", got, want)
	}
	if _, err := events(strings.NewReader("{\"Action\":\"pass\"}\nnot json\n")); err == nil {
		t.Error("events read a line that is not an event, and want an error")
	}
}

func keys(ks []key) []string {
	var out []string
	for _, k := range ks {
		out = append(out, k.String())
	}
	return out
}

// What it costs when this fails: licenses' ignore of psx, which goes by prefix, passes a package of
// another module under psx's path.
func TestOutsideNamesAPackageUnderThePathOfAnotherModule(t *testing.T) {
	t.Parallel()
	const psx = "kernel.org/pub/linux/libs/security/libcap/psx"
	list := psx + " " + psx + "\n" +
		psx + "/sub " + psx + "\n" +
		psx + "x/evil example.com/evil\n" +
		"github.com/birdtable/go-proc/proc github.com/birdtable/go-proc\n"
	if got, want := outside(list, psx), []string{psx + "x/evil example.com/evil"}; !slices.Equal(got, want) {
		t.Errorf("outside is %q, and want %q", got, want)
	}
}

// What it costs when this fails: the layers step reads fewer want lines, rows or rules than the
// files hold, and a layer row that stopped refusing passes as proved red.
func TestLayersReadsTheWantLinesRowsAndRules(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"gate/layers/testdata/proc/plant.go":       {Data: []byte("package proc\n\nimport (\n\t_ \"x/load\" // want layers-proc\n)\n")},
		"gate/layers/testdata/proc/plant_test.go":  {Data: []byte("package proc\n\nimport _ \"x/sandbox\" // want layers-proc-tests\n")},
		"gate/layers/testdata/norow/plant.go":      {Data: []byte("package norow\r\n\r\nimport _ \"x\" // want layers-no-row\r\n")},
		"gate/layers/testdata/internal/a/plant.go": {Data: []byte("package a\n\nimport _ \"y\" // a comment // want layers-internal/a\n")},
		"gate/layers/testdata/README":              {Data: []byte("// want nothing, not Go\n")},
	}
	want, err := wants(fsys, plants)
	if err != nil {
		t.Fatal(err)
	}
	if w := []string{
		"gate/layers/testdata/internal/a/plant.go:3 layers-internal/a",
		"gate/layers/testdata/norow/plant.go:3 layers-no-row",
		"gate/layers/testdata/proc/plant.go:4 layers-proc",
		"gate/layers/testdata/proc/plant_test.go:3 layers-proc-tests",
	}; !slices.Equal(want, w) {
		t.Errorf("wants are %q, and want %q", want, w)
	}
	if got, w := planted(folders(want)), []string{"internal/a", "proc"}; !slices.Equal(got, w) {
		t.Errorf("planted is %q, and want %q", got, w)
	}
	if got, w := rows("rows:\n  - path: proc\n  - path: internal/a\r\n    allow: [x]\n"), []string{"internal/a", "proc"}; !slices.Equal(got, w) {
		t.Errorf("rows are %q, and want %q", got, w)
	}
	golangci := strings.Join([]string{
		"linters:",
		"  settings:",
		"    depguard:",
		"      rules:",
		"        layers-proc:",
		"        layers-proc-tests:",
		"        layers-internal/a:",
		"        layers-internal/a-tests:",
		"        layers-no-row:",
		"          files:",
		`            - "!**/proc/*.go"`,
		`            - "!**/internal/a/*.go"`,
		"        owner-exec:",
		"",
	}, "\n")
	code, tests, norow := rules(golangci)
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"code", code, []string{"internal/a", "proc"}},
		{"tests", tests, []string{"internal/a", "proc"}},
		{"norow", norow, []string{"internal/a", "proc"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s rules are %q, and want %q", c.name, c.got, c.want)
		}
	}
	out := "level=info msg=x\n" +
		`gate\layers\testdata\proc\plant.go:4:4: import 'x/load' is not allowed from list 'layers-proc' (depguard)` + "\r\n" +
		"gate/layers/testdata/norow/plant.go:3:8: import 'x' is not allowed from list 'layers-no-row' (depguard)\n" +
		"gate/layers/testdata/proc/plant.go:9:1: some other linter\n" +
		"2 issues:\n"
	if got, w := issues(out), []string{
		"gate/layers/testdata/norow/plant.go:3 layers-no-row",
		"gate/layers/testdata/proc/plant.go:4 layers-proc",
	}; !slices.Equal(got, w) {
		t.Errorf("issues are %q, and want %q", got, w)
	}
	var errs bytes.Buffer
	if err := differ(&errs, "x", "want", []string{"a"}, "got", []string{"a", "b"}); err == nil || !strings.Contains(errs.String(), "got:\na\nb\n") {
		t.Errorf("differ on unequal lists is %v and said %q, and want errRed and both lists", err, &errs)
	}
	if err := differ(&errs, "x", "want", []string{"a"}, "got", []string{"a"}); err != nil {
		t.Errorf("differ on equal lists is %v, and want nil", err)
	}
}
