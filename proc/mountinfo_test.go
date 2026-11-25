package proc

import (
	"errors"
	"slices"
	"syscall"
	"testing"
)

// TestCgroupMountsSplitsAMountinfoLineAtItsSpacesAlone: cgroupMounts, given mountinfo lines, gives
// the mount point of each cgroup v2 and v1 mount, in order, and of no other. A carriage return, a
// vertical tab, a form feed, U+0085, U+00A0 or U+2028 in a mount point stays in it, and one in the
// root field moves no field; \040, \011, \012 and \134 in either field read back as space, tab,
// newline and \ in the mount point; a last line with no newline counts; a line with too few fields,
// or no - after the sixth, is EINVAL. It runs on every system.
//
// What it costs when this fails: the shim covers a cgroup mount whose mount point holds such a
// character at the path cut there, an unrelated folder, and leaves the real mount open by its own
// path, from which the tree freezes or kills its caller and then lifts its own limit; or it reads a mount it should not, and refuses every run.
func TestCgroupMountsSplitsAMountinfoLineAtItsSpacesAlone(t *testing.T) {
	t.Parallel()
	line := func(root, point, kind string) string {
		return "35 24 0:30 " + root + " " + point + " rw,nosuid,nodev,noexec,relatime shared:9 - " + kind + " " + kind + " rw,nsdelegate\n"
	}
	odd := []string{"\r", "\v", "\f", "\u0085", " ", " "}
	for _, c := range odd {
		for _, kind := range []string{"cgroup2", "cgroup"} {
			point := "/srv/a" + c + "b/sys/fs/cgroup"
			if got, err := cgroupMounts(line("/", point, kind)); err != nil || !slices.Equal(got, []string{point}) {
				t.Errorf("a %s mount at %q: %q, %v, want that point", kind, point, got, err)
			}
			if got, err := cgroupMounts(line("/a"+c+"b", "/sys/fs/cgroup", kind)); err != nil || !slices.Equal(got, []string{"/sys/fs/cgroup"}) {
				t.Errorf("a %s mount of root %q: %q, %v, want /sys/fs/cgroup", kind, "/a"+c+"b", got, err)
			}
		}
	}
	for esc, raw := range map[string]string{`\040`: " ", `\011`: "\t", `\012`: "\n", `\134`: `\`} {
		if got, err := cgroupMounts(line("/", "/srv/a"+esc+"b", "cgroup2")); err != nil || !slices.Equal(got, []string{"/srv/a" + raw + "b"}) {
			t.Errorf("a mount point holding %s: %q, %v, want %q", esc, got, err, "/srv/a"+raw+"b")
		}
		if got, err := cgroupMounts(line("/a"+esc+"b", "/sys/fs/cgroup", "cgroup2")); err != nil || !slices.Equal(got, []string{"/sys/fs/cgroup"}) {
			t.Errorf("a root holding %s: %q, %v, want /sys/fs/cgroup", esc, got, err)
		}
	}
	table := line("/", "/sys/fs/cgroup", "cgroup2") +
		"22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n" +
		"40 35 0:33 / /sys/fs/cgroup/memory rw,relatime - cgroup cgroup rw,memory\n" +
		"41 1 0:34 / /tmp/cgroup2 rw - cgroup2 none rw"
	if got, err := cgroupMounts(table); err != nil || !slices.Equal(got, []string{"/sys/fs/cgroup", "/sys/fs/cgroup/memory", "/tmp/cgroup2"}) {
		t.Errorf("a table of four mounts, three cgroup ones, the last with no newline: %q, %v, want the three", got, err)
	}
	for _, bad := range []string{
		"35 24 0:30 / /sys/fs/cgroup rw - cgroup2\n",
		"35 24 0:30 / /sys/fs/cgroup rw shared:9 cgroup2 cgroup2 rw\n",
		"35 24 0:30 / /sys/fs/cgroup\n",
	} {
		if got, err := cgroupMounts(bad); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("the line %q: %q, %v, want EINVAL", bad, got, err)
		}
	}
}

// TestUncoveredCgroupsReadsTheShimsCoversByMountAndParentId: uncoveredCgroups, given mountinfo and the root of the run's cgroup,
// gives the mount point of each cgroup mount that is open, and none of one that is covered: a
// cgroup v2 whose parent is a tmpfs at its point, whose parent is a cgroup mount at it. It reads
// only the cover's root, which must be the run's, so a bind of the caller's own mount back over a tmpfs, whose root is the caller's, is open; a cgroup v2
// mounted directly over the caller's, with no tmpfs, a cover at another point, and a cgroup v1 at the
// top, are open, as is one of two mounts that is open beside a covered one. A mount point's escapes
// read back. A line that is no mountinfo line is EINVAL. It runs on every system.
//
// What it costs when this fails: the shim's check passes a tree that keeps the caller's
// cgroup mounts, or refuses every run the shim covered.
func TestUncoveredCgroupsReadsTheShimsCoversByMountAndParentId(t *testing.T) {
	t.Parallel()
	row := func(id, parent, root, point, kind string) string {
		return id + " " + parent + " 0:30 " + root + " " + point + " rw - " + kind + " " + kind + " rw\n"
	}
	held := row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup", "tmpfs") + row("91", "90", "/run", "/sys/fs/cgroup", "cgroup2")
	for _, c := range []struct {
		name, info string
		want       []string
	}{
		{"no mount", "", nil},
		{"the shim's cover", held, nil},
		{"the cover's shape with the caller's root, a bind back", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup", "tmpfs") + row("91", "90", "/", "/sys/fs/cgroup", "cgroup2"), []string{"/sys/fs/cgroup"}},
		{"the cover, its root another cgroup's", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup", "tmpfs") + row("91", "90", "/other", "/sys/fs/cgroup", "cgroup2"), []string{"/sys/fs/cgroup"}},
		{"the cover with the run's root", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup", "tmpfs") + row("91", "90", "/run", "/sys/fs/cgroup", "cgroup2"), nil},
		{"the caller's mount open", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2"), []string{"/sys/fs/cgroup"}},
		{"a cgroup v2 over it with no tmpfs between", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("91", "35", "/", "/sys/fs/cgroup", "cgroup2"), []string{"/sys/fs/cgroup"}},
		{"a tmpfs over it and no cgroup v2 on that", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup", "tmpfs"), nil},
		{"a tmpfs at another point under a cgroup v2", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup/x", "tmpfs") + row("91", "90", "/run", "/sys/fs/cgroup/x", "cgroup2"), []string{"/sys/fs/cgroup", "/sys/fs/cgroup/x"}},
		{"a cgroup v1 on the cover", row("35", "24", "/", "/sys/fs/cgroup", "cgroup2") + row("90", "35", "/", "/sys/fs/cgroup", "tmpfs") + row("91", "90", "/run", "/sys/fs/cgroup", "cgroup"), []string{"/sys/fs/cgroup"}},
		{"one point covered and one open", held + row("40", "24", "/", "/sys/fs/cgroup/unified", "cgroup2"), []string{"/sys/fs/cgroup/unified"}},
		{"a mount point with an escaped space", row("35", "24", "/", `/srv/a\040b`, "cgroup2"), []string{"/srv/a b"}},
	} {
		got, err := uncoveredCgroups(c.info, "/run")
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, %v, want %q", c.name, got, err, c.want)
		}
	}
	if _, err := uncoveredCgroups("a line\n", "/run"); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("a line that is no mountinfo line: %v, want EINVAL", err)
	}
}

// TestCoverRootIsTheRunsCgroupAsTheCallerReadsIt: coverRoot, given the caller's mountinfo and the
// run's cgroup, gives the root of the cgroup v2 mount whose point is the deepest folder above the run's
// cgroup joined with the rest of the path: / and a run at /sys/fs/cgroup/a/b is /a/b; a mount of root
// /x at the point and a run below it is /x/a/b; a deeper mount wins; a root of "/.." parts drops as many leading parts of the rest (root /.. at the mount
// and a run at p/run below it is /run), and a rest with fewer parts is EINVAL; the mount itself is its root; a
// path beside the point, not below it, is no match. No cgroup v2 mount above it, or a line that is no
// mountinfo line, is EINVAL. It runs on every system.
//
// What it costs when this fails: the cover's root is compared with another folder, and every run is
// refused, or the caller's own root passes for the run's.
func TestCoverRootIsTheRunsCgroupAsTheCallerReadsIt(t *testing.T) {
	t.Parallel()
	row := func(id, root, point, kind string) string {
		return id + " 24 0:30 " + root + " " + point + " rw - " + kind + " " + kind + " rw\n"
	}
	for _, c := range []struct{ name, info, run, want string }{
		{"a mount of root /", row("35", "/", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup/a/b", "/a/b"},
		{"a mount of root /x", row("35", "/x", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup/a/b", "/x/a/b"},
		{"the deeper mount", row("35", "/", "/sys/fs/cgroup", "cgroup2") + row("36", "/y", "/sys/fs/cgroup/a", "cgroup2"), "/sys/fs/cgroup/a/b", "/y/b"},
		{"the mount itself", row("35", "/x", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup", "/x"},
		{"a point at the root of the tree", row("35", "/", "/", "cgroup2"), "/a/b", "/a/b"},
		{"a root above the caller's namespace", row("35", "/..", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup/p/run", "/run"},
		{"a root two above, a run two down", row("35", "/../..", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup/p/q/run", "/run"},
		{"a root above and a part of its own", row("35", "/../x", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup/p/run", "/x/run"},
	} {
		if got, err := coverRoot(c.info, c.run); err != nil || got != c.want {
			t.Errorf("%s: %q, %v, want %q", c.name, got, err, c.want)
		}
	}
	for name, c := range map[string]struct{ info, run string }{
		"a path beside the point":              {row("35", "/", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup2/a"},
		"a cgroup v1 alone":                    {row("35", "/", "/sys/fs/cgroup", "cgroup"), "/sys/fs/cgroup/a"},
		"no mount":                             {"", "/sys/fs/cgroup/a"},
		"no mountinfo line":                    {"a line\n", "/sys/fs/cgroup/a"},
		"a run outside the caller's namespace": {row("35", "/../..", "/sys/fs/cgroup", "cgroup2"), "/sys/fs/cgroup/a"},
	} {
		if got, err := coverRoot(c.info, c.run); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%s: %q, %v, want EINVAL", name, got, err)
		}
	}
}
