package proc

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// cgroupMounts is the mount point of each cgroup v2 or v1 mount that mountinfo, a mountinfo file,
// lists, in its order. The kernel parts a line's fields by one space, and escapes, as \ and three
// octal digits, only space, tab, newline and \ in a path: so a line is split at each space alone,
// and a path keeps every other byte, a carriage return or a Unicode space among them. A line that is no mountinfo line is EINVAL.
func cgroupMounts(mountinfo string) ([]string, error) {
	var points []string
	for line := range strings.Lines(mountinfo) {
		// id, parent, device, root, mount point, options, optional fields, -, type, source, superblock options
		f := strings.Split(strings.TrimSuffix(line, "\n"), " ")
		i := 6 + slices.Index(f[min(6, len(f)):], "-")
		if len(f) < 10 || i < 6 || len(f) < i+3 {
			return nil, syscall.EINVAL
		}
		if f[i+1] == "cgroup2" || f[i+1] == "cgroup" {
			points = append(points, mountPoint(f[4]))
		}
	}
	return points, nil
}

// mountPoint is s, a mount point as mountinfo writes it, with each byte it escapes, as \ and three
// octal digits (space, tab, newline and \ itself), back.
func mountPoint(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountRow is the fields of a mountinfo line that the covers check reads.
type mountRow struct{ id, parent, root, point, kind string }

// mountRows is the rows of mountinfo, a mountinfo file, in its order, a root and a mount point with
// each escape (mountPoint) read back. A line that is no mountinfo line is EINVAL.
func mountRows(mountinfo string) ([]mountRow, error) {
	var rows []mountRow
	for line := range strings.Lines(mountinfo) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), " ")
		i := 6 + slices.Index(f[min(6, len(f)):], "-")
		if len(f) < 10 || i < 6 || len(f) < i+3 {
			return nil, syscall.EINVAL
		}
		rows = append(rows, mountRow{f[0], f[1], mountPoint(f[3]), mountPoint(f[4]), f[i+1]})
	}
	return rows, nil
}

// coverRoot is the root that mountinfo, the caller's own, shows for a cgroup v2 mounted at the run's
// cgroup, run: that of the cgroup v2 mount whose point is the deepest folder above path, joined
// with the rest of run. The shim's cover is a cgroup v2 of the tree's namespace, rooted at the
// run's cgroup, and the caller, who reads it, sees that root; a bind of the caller's own mount
// back over a tmpfs shows the caller's root instead. No such mount is EINVAL.
func coverRoot(mountinfo, run string) (string, error) {
	rows, err := mountRows(mountinfo)
	if err != nil {
		return "", err
	}
	best, found := mountRow{}, false
	for _, r := range rows {
		if r.kind == "cgroup2" && (run == r.point || strings.HasPrefix(run, strings.TrimSuffix(r.point, "/")+"/")) && (!found || len(r.point) >= len(best.point)) {
			best, found = r, true
		}
	}
	if !found {
		return "", syscall.EINVAL
	}
	// A root above the caller's cgroup namespace shows leading ".." parts, and the kernel shows a cgroup
	// below the mount relative to the reader's namespace root, which every run go-proc places lies
	// under: the first parts of the rest cancel the root's "..", so as many are dropped as the root has
	// A rest with fewer parts is a run outside the caller's
	// namespace, which the kernel does not let the caller place, and is EINVAL.
	rest := strings.FieldsFunc(strings.TrimPrefix(run, best.point), func(r rune) bool { return r == '/' })
	parts := strings.FieldsFunc(best.root, func(r rune) bool { return r == '/' })
	up := 0
	for up < len(parts) && parts[up] == ".." {
		up++
	}
	if len(rest) < up {
		return "", syscall.EINVAL
	}
	return path.Join("/", path.Join(parts[up:]...), path.Join(rest[up:]...)), nil
}

// uncoveredCgroups is the mount point of each cgroup v2 or v1 mount in mountinfo that is open, with
// nothing mounted over it at its point, and is not the cover the shim puts there: a cgroup v2 of
// root cover whose parent is a tmpfs at the same point, whose parent is a cgroup mount at the same
// point (shimSteps' coverCgroups). It reads by mount id and parent id, and compares only the
// cover's root, as the caller shows it (coverRoot): a root is shown from the reader's cgroup
// namespace, and the reader is always the caller. A line that is no mountinfo line is
// EINVAL.
func uncoveredCgroups(mountinfo, cover string) ([]string, error) {
	rows, err := mountRows(mountinfo)
	if err != nil {
		return nil, err
	}
	byID := map[string]mountRow{}
	over := map[string]bool{} // the ids with a mount over them at their own point
	for _, r := range rows {
		byID[r.id] = r
	}
	for _, r := range rows {
		if p, ok := byID[r.parent]; ok && p.point == r.point {
			over[p.id] = true
		}
	}
	isCgroup := func(r mountRow) bool { return r.kind == "cgroup2" || r.kind == "cgroup" }
	var open []string
	for _, r := range rows {
		if !isCgroup(r) || over[r.id] {
			continue
		}
		tmp, ok := byID[r.parent]
		below, ok2 := byID[tmp.parent]
		if r.kind != "cgroup2" || r.root != cover || !ok || !ok2 || tmp.kind != "tmpfs" || tmp.point != r.point || !isCgroup(below) || below.point != r.point {
			open = append(open, r.point)
		}
	}
	return open, nil
}
