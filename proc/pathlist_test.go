package proc

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pathListCase is a list of folders PathList is given on a system, and the value it gives, or the
// place in dirs of the folder it refuses (refused is -1 where it joins them).
type pathListCase struct {
	name    string
	dirs    []string
	want    string
	refused int
}

// unixPathLists are the cases on every system but windows.
func unixPathLists() []pathListCase {
	return []pathListCase{
		{"three folders, one relative and one with a space", []string{"/usr/bin", "/opt/a b/bin", "rel/bin"}, "/usr/bin:/opt/a b/bin:rel/bin", -1},
		{"; and a quote are a name's own here", []string{`/a;b`, `/c"d`}, `/a;b:/c"d`, -1},
		{"a repeat is written as given", []string{"/a", "/a/"}, "/a:/a/", -1},
		{"one folder", []string{"/usr/bin"}, "/usr/bin", -1},
		{"no folders", nil, "", -1},
		{"a folder holding the separator", []string{"/a", "/b:c"}, "", 1},
		{"an empty name", []string{"/a", ""}, "", 1},
		{"a name holding a NUL", []string{"/a\x00b", "/c"}, "", 0},
	}
}

// windowsPathLists are the cases on windows.
func windowsPathLists() []pathListCase {
	return []pathListCase{
		{"three folders, one relative and one with a space", []string{`C:\Windows`, `C:\Program Files\a`, `rel\bin`}, `C:\Windows;C:\Program Files\a;rel\bin`, -1},
		{": is a drive's own here", []string{`C:\a`, `D:b`}, `C:\a;D:b`, -1},
		{"a repeat is written as given", []string{`C:\a`, `c:\A\`}, `C:\a;c:\A\`, -1},
		{"one folder", []string{`C:\Windows`}, `C:\Windows`, -1},
		{"no folders", nil, "", -1},
		{"a folder holding the separator", []string{`C:\a;b`, `C:\c`}, "", 0},
		{"a folder holding a quote", []string{`C:\a`, `C:\"b"`}, "", 1},
		{"a folder holding a lone quote", []string{`C:\a"`}, "", 0},
		{"an empty name", []string{"", `C:\a`}, "", 0},
		{"a name holding a NUL", []string{`C:\a`, "C:\\b\x00"}, "", 1},
	}
}

// TestPathListIsEachSystemsJoin: PathList joins with ; on windows and with : on every other system,
// as FileName reads goos, refusing by ErrPathElement, and by the folder's name and place, the folders
// the system's PATH cannot hold as one element, and joining nothing then. Each value it gives splits
// back to its folders on the separator alone; on this host, filepath.SplitList of it does too.
//
// What it costs when this fails: a child is handed a PATH it reads as other folders than the caller
// kept, one of them relative or unfiltered, and a program started by name is found there; or a
// caller's filter and the call disagree, and a folder the caller kept is refused at the tag.
func TestPathListIsEachSystemsJoin(t *testing.T) {
	t.Parallel()
	for _, sys := range []struct {
		goos, sep string
		cases     []pathListCase
	}{
		{"linux", ":", unixPathLists()},
		{"darwin", ":", unixPathLists()},
		{"freebsd", ":", unixPathLists()},
		{"Windows", ":", unixPathLists()},
		{"windows", ";", windowsPathLists()},
	} {
		for _, tc := range sys.cases {
			got, err := PathList(sys.goos, tc.dirs...)
			if tc.refused >= 0 {
				named := fmt.Sprintf("dirs[%d], %q", tc.refused, tc.dirs[tc.refused])
				if !errors.Is(err, ErrPathElement) || !strings.Contains(err.Error(), named) || got != "" {
					t.Errorf("%s, %s: PathList(%q, %q) is %q, %v, want nothing joined and ErrPathElement naming %s", sys.goos, tc.name, sys.goos, tc.dirs, got, err, named)
				}
				continue
			}
			if err != nil || got != tc.want {
				t.Errorf("%s, %s: PathList(%q, %q) is %q, %v, want %q", sys.goos, tc.name, sys.goos, tc.dirs, got, err, tc.want)
				continue
			}
			if split := strings.Split(got, sys.sep); len(tc.dirs) > 0 && !slices.Equal(split, tc.dirs) {
				t.Errorf("%s, %s: %q split on %q alone is %q, want %q", sys.goos, tc.name, got, sys.sep, split, tc.dirs)
			}
			if sys.goos == hostGOOS() && !slices.Equal(filepath.SplitList(got), tc.dirs) {
				t.Errorf("%s, %s: filepath.SplitList(%q) is %q, want %q", sys.goos, tc.name, got, filepath.SplitList(got), tc.dirs)
			}
		}
	}
}

// LocalPathList gives what PathList gives for the system the test runs on, and filepath.SplitList
// here reads its value back as the folders it was given.
//
// What it costs when this fails: a caller that may not name the system hands a
// child a PATH joined by another system's rule.
func TestLocalPathListIsThePathListOfThisSystem(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	for _, dirs := range [][]string{
		nil,
		{tmp},
		{filepath.Join(tmp, "a b"), filepath.Join("rel", "bin"), tmp, tmp},
		{tmp, ""},
		{tmp + string(filepath.ListSeparator) + "b"},
	} {
		got, err := LocalPathList(dirs...)
		want, wantErr := PathList(hostGOOS(), dirs...)
		if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
			t.Errorf("LocalPathList(%q) is %q, %v, and PathList(%q, ...) is %q, %v", dirs, got, err, hostGOOS(), want, wantErr)
		}
		if err == nil && !slices.Equal(filepath.SplitList(got), dirs) {
			t.Errorf("filepath.SplitList(LocalPathList(%q)) is %q", dirs, filepath.SplitList(got))
		}
	}
}

// TestAChildGivenALocalPathListReadsItsFolders: a program Run starts with PATH set from
// LocalPathList, and no other PATH in its Env in any case of the name, reads by filepath.SplitList
// the folders it was given, in order, a repeat and a relative one with them; on Linux and macOS one
// holding ; and a quote too. It waits on no deadline, so it runs in every tier, the release tier's
// run on each system with them.
//
// What it costs when this fails: the value is right on paper and not as a child reads it, and a
// program the child starts by name is looked up in folders the caller did not keep.
func TestAChildGivenALocalPathListReadsItsFolders(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	dirs := []string{filepath.Join(tmp, "a b"), filepath.Join(tmp, "c=d"), filepath.Join("rel", "bin"), filepath.Join(tmp, "a b")}
	if hostGOOS() != "windows" {
		dirs = append(dirs, filepath.Join(tmp, `e;f"g`))
	}
	value, err := LocalPathList(dirs...)
	if err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "path-split")
	c.Env = slices.DeleteFunc(c.Env, func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(name, "PATH")
	})
	c.Env = append(c.Env, "PATH="+value)
	r, err := Run(t.Context(), c)
	if err != nil || r.Code != 0 {
		t.Fatalf("the child: %v, code %d, stderr %q", err, r.Code, r.Stderr)
	}
	if got := strings.Split(strings.TrimSuffix(string(r.Stdout), "\x00"), "\x00"); !slices.Equal(got, dirs) {
		t.Errorf("the child given PATH=%q read %q, want %q", value, got, dirs)
	}
}
