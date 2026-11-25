package workflow

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// file is the release workflow, three folders up from this package's and down again.
const file = "../../.github/workflows/release.yml"

// The three shapes a line may read the ref input in: a checkout's ref, the check's environment, and
// the first term of the release job's if.
const (
	checkoutRef = "ref: ${{ inputs.ref }}"
	checkEnv    = "REF: ${{ inputs.ref }}"
	releaseIf   = "if: inputs.ref == '' && "
)

// check is fetch's first step, line for line. Its script was run by hand with an empty value, a full
// sha and eight values that are neither, so a change to
// any line of it is red here until it is run again and this copy follows.
func check() []string {
	return []string{
		"- name: ref is empty or a full commit sha",
		"env:",
		checkEnv,
		"LC_ALL: C",
		"run: |",
		`refuse() { echo "ref is not a full commit sha, so nothing runs"; exit 1; }`,
		`case "$REF" in`,
		"'') ;;",
		"*[!0-9a-f]*) refuse ;;",
		`*) if [ "${#REF}" -ne 40 ]; then refuse; fi ;;`,
		"esac",
	}
}

// read is the workflow's text with each line ending in a line feed alone, whatever the checkout
// wrote.
func read(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// jobs reads the workflow's jobs: each one's name, in the file's order, and its lines, trimmed, with
// no comment line and no empty one.
func jobs(src string) (names []string, lines map[string][]string) {
	lines = map[string][]string{}
	in, name := false, ""
	for l := range strings.SplitSeq(src, "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case l == "jobs:":
			in = true
		case !in:
		case strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") && strings.HasSuffix(l, ":"):
			name = strings.TrimSuffix(trimmed, ":")
			names = append(names, name)
		case name != "":
			lines[name] = append(lines[name], trimmed)
		}
	}
	return names, lines
}

// steps cuts a job's lines into its steps, each starting at its "- " line.
func steps(job []string) [][]string {
	var out [][]string
	at := slices.Index(job, "steps:")
	if at < 0 {
		return nil
	}
	for _, l := range job[at+1:] {
		if strings.HasPrefix(l, "- ") {
			out = append(out, nil)
		}
		if len(out) != 0 {
			out[len(out)-1] = append(out[len(out)-1], l)
		}
	}
	return out
}

// faults says where a workflow would, on a run with a ref, publish, run a tree that is not the ref's,
// or run three trees; none when it does none of them.
func faults(src string) []string {
	var out []string
	names, lines := jobs(src)
	if !slices.Equal(names, []string{"fetch", "linux", "windows", "release"}) {
		out = append(out, "the jobs are "+strings.Join(names, ", ")+", not fetch, linux, windows, release: a new job is judged here first")
	}
	for _, name := range []string{"fetch", "linux", "windows"} {
		checkouts := 0
		for _, step := range steps(lines[name]) {
			if !strings.HasPrefix(step[0], "- uses: actions/checkout@") {
				continue
			}
			checkouts++
			if !slices.Contains(step, checkoutRef) {
				out = append(out, name+" has a checkout that does not take the ref")
			}
		}
		if checkouts != 1 {
			out = append(out, name+" does not check the tree out once")
		}
		if name != "fetch" && !slices.Contains(lines[name], "needs: fetch") {
			out = append(out, name+" does not need fetch, so it runs where fetch refused the ref")
		}
	}
	if first := steps(lines["fetch"]); len(first) == 0 || !slices.Equal(first[0], check()) {
		out = append(out, "fetch's first step is not the ref check, line for line")
	}
	release := lines["release"]
	if !slices.Contains(release, "needs: [linux, windows]") {
		out = append(out, "release does not need both tiers")
	}
	if !slices.ContainsFunc(release, func(l string) bool { return strings.HasPrefix(l, releaseIf) }) {
		out = append(out, "release's if does not start with an empty ref, so a run with a ref publishes")
	}
	if slices.ContainsFunc(release, func(l string) bool { return strings.HasPrefix(l, "ref:") }) {
		out = append(out, "release checks out a ref, so it publishes a tree that is not the tag's")
	}
	for l := range strings.SplitSeq(src, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") || !strings.Contains(l, "inputs.ref") {
			continue
		}
		if l != checkoutRef && l != checkEnv && !strings.HasPrefix(l, releaseIf) {
			out = append(out, "the ref is read outside a checkout, the check's environment and release's if: "+l)
		}
		if strings.Count(l, "inputs.ref") != 1 {
			out = append(out, "a line reads the ref twice: "+l)
		}
	}
	return out
}

// A run of the release workflow with a ref checks that commit out in fetch and in both tier jobs,
// after fetch's first step has refused anything but a full sha, and its release job does not run.
//
// What it costs when this fails: a run made to prove a pull request's head before its tag publishes
// a release, or proves a tree that is not the head it names, and a tag stands on that run.
func TestARunWithARefRunsThatCommitAndPublishesNothing(t *testing.T) {
	t.Parallel()
	src := read(t)
	if got := faults(src); len(got) != 0 {
		t.Errorf("the release workflow breaks what a run with a ref is for:\n%s", strings.Join(got, "\n"))
	}
	if names, _ := jobs(src); len(names) < 4 {
		t.Fatalf("read %d jobs, so this proves little", len(names))
	}
}

// The check goes red, on the workflow itself with one thing changed: a tier's checkout without the
// ref, a tier that does not wait for fetch, the check's length taken off, moved after the checkout or
// read from a pasted value, a release without the empty-ref term or with a ref of its own, and a
// fifth job.
//
// What it costs when this fails: the check reads the workflow and never refuses, which is a check
// nothing could turn red.
func TestTheCheckRefusesAWorkflowThatWouldPublishOrRunAnotherTree(t *testing.T) {
	t.Parallel()
	src := read(t)
	tier := "          " + checkoutRef + "\n          fetch-depth: 0\n"
	for name, tc := range map[string]struct {
		from, to string
		count    int
		want     string
	}{
		"a tier's checkout without the ref": {tier, "          fetch-depth: 0\n", 2, "windows has a checkout that does not take the ref"},
		"fetch's checkout without the ref":  {"          " + checkoutRef + "\n          persist-credentials: false\n", "          persist-credentials: false\n", 1, "fetch has a checkout that does not take the ref"},
		"a tier that does not wait":         {"    needs: fetch\n    runs-on: windows-latest\n", "    runs-on: windows-latest\n", 1, "windows does not need fetch"},
		"the length is not checked":         {`*) if [ "${#REF}" -ne 40 ]; then refuse; fi ;;`, "*) ;;", 1, "fetch's first step is not the ref check"},
		"the check has a name of its own":   {"      - name: ref is empty or a full commit sha\n", "      - name: ref\n", 1, "fetch's first step is not the ref check"},
		"the value is pasted into a script": {`case "$REF" in`, `case "${{ inputs.ref }}" in`, 1, "the ref is read outside a checkout"},
		"release without the empty ref":     {"    " + releaseIf, "    if: ", 1, "release's if does not start with an empty ref"},
		"release with a ref of its own":     {"    permissions:\n      contents: write\n", "    permissions:\n      contents: write\n    ref: x\n", 1, "release checks out a ref"},
		"a fifth job":                       {"\n  release:\n", "\n  extra:\n    runs-on: ubuntu-latest\n\n  release:\n", 1, "a new job is judged here first"},
	} {
		if got := strings.Count(src, tc.from); got != tc.count {
			t.Errorf("%s: the workflow holds the line to change %d times, want %d, so the row changes nothing it names", name, got, tc.count)
			continue
		}
		at := strings.LastIndex(src, tc.from)
		broken := src[:at] + tc.to + src[at+len(tc.from):]
		got := faults(broken)
		if !slices.ContainsFunc(got, func(f string) bool { return strings.Contains(f, tc.want) }) {
			t.Errorf("%s: faults %q, want one that says %q", name, got, tc.want)
		}
	}
}
