package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// noticeFile is the NOTICE's name, at the module's root.
const noticeFile = "NOTICE"

// modulePath is a module path as NOTICE's prose writes one: a first element with a dot in it, a
// host, then one or more elements, as golang.org/x/sys or github.com/shirou/gopsutil/v4. A URL is
// not one, as its first element ends with a colon.
var modulePath = regexp.MustCompile(`(?:^|[\s(])([a-z0-9-]+(?:\.[a-z0-9-]+)+(?:/[A-Za-z0-9._~+-]*[A-Za-z0-9_~+-])+)`)

// notice says whether NOTICE in root names each module of the build at the version go.mod
// resolves, and no module go.mod does not list. in is what the Taskfile's
// notice-check task gives it: each module of the non-test build of ./... on linux, darwin and
// windows as `go list -deps` prints it, one "<path> <version>" to a line, the module's own line
// with no version among them. NOTICE's prose and its licence names are hand-written and not read;
// task licenses is the licence authority.
func notice(root *os.Root, in io.Reader) (string, int) {
	if in == nil {
		return "measure notice-check reads go list's modules from its standard input, and has none\n", 2
	}
	text, err := root.ReadFile(noticeFile)
	if err != nil {
		return err.Error() + "\n", 2
	}
	mod, err := root.ReadFile("go.mod")
	if err != nil {
		return err.Error() + "\n", 2
	}
	self, required := requires(string(mod))
	built, err := buildModules(in, self)
	if err != nil {
		return err.Error() + "\n", 2
	}
	if len(built) == 0 {
		return "measure notice-check read no module from go list, so it checked nothing\n", 2
	}
	prose := " " + strings.Join(strings.Fields(string(text)), " ") + " "
	var faults []string
	for _, m := range built {
		if !named(prose, m) {
			faults = append(faults, fmt.Sprintf("%s is in the build and NOTICE does not name it as %q", m[0], m[0]+" "+m[1]))
		}
	}
	for _, p := range namedPaths(prose) {
		if p != self && !slices.Contains(required, p) {
			faults = append(faults, fmt.Sprintf("NOTICE names %s, which go.mod does not list", p))
		}
	}
	if len(faults) == 0 {
		return "", 0
	}
	return "NOTICE disagrees with go.mod and the build:\n" + strings.Join(faults, "\n") + "\n", 1
}

// requires gives the module path go.mod declares and the path of each module it requires, from a
// block or a line of its own.
func requires(mod string) (string, []string) {
	var self string
	var out []string
	block := false
	for line := range strings.SplitSeq(mod, "\n") {
		f := strings.Fields(strings.TrimSpace(strings.SplitN(line, "//", 2)[0]))
		switch {
		case len(f) == 0:
		case block && f[0] == ")":
			block = false
		case block:
			out = append(out, f[0])
		case f[0] == "module" && len(f) > 1:
			self = strings.Trim(f[1], `"`)
		case f[0] == "require" && len(f) > 1 && f[1] == "(":
			block = true
		case f[0] == "require" && len(f) > 2:
			out = append(out, f[1])
		}
	}
	return self, out
}

// buildModules reads go list's lines from in: each module but self, once, as its path and its
// version, in the order first read.
func buildModules(in io.Reader, self string) ([][2]string, error) {
	var out [][2]string
	s := bufio.NewScanner(in)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) == 0 || f[0] == self {
			continue
		}
		if len(f) != 2 {
			return nil, fmt.Errorf("measure notice-check reads %q from go list, and wants a module's path and its version", s.Text())
		}
		m := [2]string{f[0], f[1]}
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out, s.Err()
}

// named is whether prose, NOTICE with each run of white space one space, names m's path then its
// version, each a word of its own.
func named(prose string, m [2]string) bool {
	want := " " + m[0] + " " + m[1]
	for i := strings.Index(prose, want); i >= 0; {
		end := i + len(want)
		if end == len(prose) || strings.ContainsRune(" ,.;:)", rune(prose[end])) {
			return true
		}
		next := strings.Index(prose[end:], want)
		if next < 0 {
			return false
		}
		i = end + next
	}
	return false
}

// namedPaths is each module path prose names, once, in the order it names them.
func namedPaths(prose string) []string {
	var out []string
	for _, m := range modulePath.FindAllStringSubmatch(prose, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}
