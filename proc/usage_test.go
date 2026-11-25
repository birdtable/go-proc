package proc

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoFileButLoadsLinuxFilesImportsGopsutil: of every Go file of the module, for every system and
// its tests among them, only load's Linux files (load/*_linux.go and load/*_linux_test.go) import a
// gopsutil package, and those only github.com/shirou/gopsutil/v4/load and
// github.com/shirou/gopsutil/v4/common. go.mod requires no gopsutil module but
// v4. depguard's os-host and os-host-sandbox rules deny gopsutil to every other file, so lint fails
// the first that adds it. Usage and Pids read a tree's job themselves.
//
// What it costs when this fails: proc reads a tree's processes through a package that lists them by
// a snapshot of every process on the system, so a pid reused by another program is reported as the
// tree's and stopped, and its dependencies come in without the intake supply.intake.5 asks for.
func TestNoFileButLoadsLinuxFilesImportsGopsutil(t *testing.T) {
	t.Parallel()
	const gopsutil = "github.com/shirou/gopsutil"
	allowed := map[string]bool{gopsutil + "/v4/load": true, gopsutil + "/v4/common": true}
	files := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != ".." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, ".."+string(filepath.Separator)))
		linuxLoad := strings.HasPrefix(rel, "load/") && strings.Count(rel, "/") == 1 &&
			(strings.HasSuffix(rel, "_linux.go") || strings.HasSuffix(rel, "_linux_test.go"))
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(p, gopsutil) {
				continue
			}
			if !linuxLoad || !allowed[p] {
				t.Errorf("%s imports %s", rel, p)
			}
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("the module's files: %d read, %v", files, err)
	}
	b, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if strings.Contains(line, gopsutil) && !strings.Contains(line, gopsutil+"/v4 ") {
			t.Errorf("go.mod requires %q, want no gopsutil module but v4", strings.TrimSpace(line))
		}
	}
	b, err = os.ReadFile("../.golangci.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"os-host:", "os-host-sandbox:"} {
		_, after, ok := strings.Cut(string(b), "\n        "+rule+"\n")
		body, _, _ := strings.Cut(after, "\n        # ")
		if !ok || !strings.Contains(body, "- pkg: github.com/shirou/gopsutil\n") {
			t.Errorf("depguard's %s rule does not deny github.com/shirou/gopsutil", strings.TrimSuffix(rule, ":"))
		}
	}
}
