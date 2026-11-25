package load

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	pscommon "github.com/shirou/gopsutil/v4/common"
)

// systemFigure is the first three fields of /proc/loadavg, read and split by the test itself.
func systemFigure(t *testing.T) [3]float64 {
	t.Helper()
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		t.Fatalf("/proc/loadavg reads %q", b)
	}
	var sys [3]float64
	for i := range sys {
		if sys[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			t.Fatalf("/proc/loadavg reads %q: %v", b, err)
		}
	}
	return sys
}

// TestTheFolderLoadavgIsReadFromIsPinnedToProc: Read's context names /proc as gopsutil's HOST_PROC,
// and gopsutil reads loadavg from the folder that context names: one holding "9.50 8.25 7.00 1/1 1"
// gives 9.5, 8.25 and 7. gopsutil asks its context key before the HOST_PROC variable, so the
// variable cannot move Read.
//
// What it costs when this fails: a HOST_PROC in the caller's environment, which a container image
// or a parent process sets, makes Read give the figures of a file of its choosing as the system's.
func TestTheFolderLoadavgIsReadFromIsPinnedToProc(t *testing.T) {
	t.Parallel()
	env, _ := pinned(context.Background()).Value(pscommon.EnvKey).(pscommon.EnvMap)
	if got := env[pscommon.HostProcEnvKey]; got != "/proc" {
		t.Errorf("Read's HOST_PROC is %q, want /proc", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "loadavg"), []byte("9.50 8.25 7.00 1/1 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), pscommon.EnvKey, pscommon.EnvMap{pscommon.HostProcEnvKey: dir})
	if got, err := avg(ctx); err != nil || got != [3]float64{9.5, 8.25, 7} {
		t.Errorf("gopsutil under a HOST_PROC of a folder holding 9.50 8.25 7.00: %v, %v, want [9.5 8.25 7]", got, err)
	}
}

// TestALoadavgOfFewerThanThreeFieldsIsAnError: under a HOST_PROC of a folder whose loadavg holds
// nothing, only a newline, one field or two, avg returns an error and the zero figures, and does
// not panic; Read wraps that error in ErrNoAverage, as it does every error of its source.
//
// What it costs when this fails: a host that masks /proc/loadavg with an empty file, as a container
// runtime can, panics every Read, and with it every test that reads the load to stretch a wait,
// as sandbox's does.
func TestALoadavgOfFewerThanThreeFieldsIsAnError(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"", "\n", "1.00\n", "1.00 2.00\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "loadavg"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(context.Background(), pscommon.EnvKey, pscommon.EnvMap{pscommon.HostProcEnvKey: dir})
		if got, err := avg(ctx); err == nil || got != [3]float64{} {
			t.Errorf("a loadavg of %q: %v, %v, want the zero figures and an error", line, got, err)
		}
	}
}
