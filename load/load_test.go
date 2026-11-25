package load_test

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// TestTheBuildImportsTheStandardLibraryAndXSysAndOnLinuxGopsutilsLoad: for macOS and Windows, `go
// list -deps` of the package names no os/exec, and no package outside the standard library but
// golang.org/x/sys. For Linux it names gopsutil's load, common and internal/common besides, and
// os/exec only as internal/common's import. The test runs go through proc, as the package's own
// code never could.
//
// What it costs when this fails: a program that imports load to read its load starts programs,
// takes over the runtime's cgo hooks (gopsutil's macOS build does, by purego), or pulls in a module
// the package's rules do not allow, without a line of its own saying so.
func TestTheBuildImportsTheStandardLibraryAndXSysAndOnLinuxGopsutilsLoad(t *testing.T) {
	t.Parallel()
	gocmd := goCommand(t)
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			res, err := proc.Run(ctx, proc.Cmd{
				Path:      gocmd,
				Args:      []string{"list", "-deps", "-f", "{{.ImportPath}} {{.Standard}} {{join .Imports \" \"}}", "."},
				Env:       append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=0"),
				Timeout:   2 * time.Minute,
				MaxOutput: 1 << 20,
				Drain:     10 * time.Second,
			})
			if err != nil || res.Code != 0 {
				t.Fatalf("go list: code %d, %v\n%s", res.Code, err, res.Stderr)
			}
			// The Linux build takes gopsutil's load and the two packages it imports;
			// internal/common imports os/exec for a call load.Avg never makes.
			var allowed []string
			if goos == "linux" {
				allowed = []string{
					"github.com/shirou/gopsutil/v4/load",
					"github.com/shirou/gopsutil/v4/common",
					"github.com/shirou/gopsutil/v4/internal/common",
				}
			}
			if bad := foreign(res.Stdout, allowed); len(bad) > 0 {
				t.Errorf("GOOS=%s: the package imports %q", goos, bad)
			}
		})
	}
}

// foreign is each package of go list's output, "<import path> <standard> <imports...>", that is
// outside the standard library, golang.org/x/sys, this one and allowed; and os/exec, unless
// gopsutil's internal/common is allowed and is the one package that imports it. go list names a
// package before its importers, so os/exec is judged once every line is read.
func foreign(out []byte, allowed []string) []string {
	const common = "github.com/shirou/gopsutil/v4/internal/common"
	var bad, execBy []string
	exec := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			bad = append(bad, sc.Text())
			continue
		}
		path, std := f[0], f[1]
		if slices.Contains(f[2:], "os/exec") {
			execBy = append(execBy, path)
		}
		switch {
		case path == "os/exec":
			exec = true
		case std == "true", strings.HasPrefix(path, "golang.org/x/sys/"),
			path == "github.com/birdtable/go-proc/load", slices.Contains(allowed, path):
		default:
			bad = append(bad, path)
		}
	}
	if exec && (!slices.Contains(allowed, common) || !slices.Equal(execBy, []string{common})) {
		bad = append(bad, "os/exec, imported by "+strings.Join(execBy, ", "))
	}
	return bad
}

// goCommand is the go command that runs the test, by its full path and file name, goExe: go
// test puts its GOROOT's bin first in a test's PATH (Go 1.19), so a test that runs go runs the
// same one.
func goCommand(t *testing.T) string {
	t.Helper()
	path, _ := os.LookupEnv("PATH")
	dirs := filepath.SplitList(path)
	if len(dirs) == 0 || !filepath.IsAbs(dirs[0]) {
		t.Fatalf("PATH %q does not start with go's bin folder", path)
	}
	return filepath.Join(dirs[0], goExe)
}
