package load

import (
	"context"
	"fmt"

	pscommon "github.com/shirou/gopsutil/v4/common"
	psload "github.com/shirou/gopsutil/v4/load"
)

// procDir is the folder gopsutil reads loadavg from. It is pinned by gopsutil's context key, which
// it asks before the HOST_PROC variable, so no variable of the caller's environment moves the
// read to another file.
const procDir = "/proc"

// pinned is ctx with gopsutil's HOST_PROC set to procDir.
func pinned(ctx context.Context) context.Context {
	return context.WithValue(ctx, pscommon.EnvKey, pscommon.EnvMap{pscommon.HostProcEnvKey: procDir})
}

// read reads the averages by gopsutil's load.Avg: /proc/loadavg, and sysinfo(2) when that file
// cannot be read or a field of it parsed. A file of fewer than three fields is an error. It starts
// no program.
func read() ([3]float64, error) {
	return avg(pinned(context.Background())) //nolint:forbidigo // go.concurrency.1: Read takes no context; this one carries the HOST_PROC pin alone, and nothing Avg does on Linux, a file read or sysinfo(2), waits on one
}

// avg is gopsutil's reading under ctx. A panic in it is an error: gopsutil v4.26.10 indexes the
// first three fields of loadavg without counting them, so a file of fewer, such as an empty one a
// container masks /proc/loadavg with, panics in place of failing.
func avg(ctx context.Context) (out [3]float64, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = [3]float64{}, fmt.Errorf("gopsutil's load.Avg failed on the source: %v", r)
		}
	}()
	a, err := psload.AvgWithContext(ctx)
	if err != nil {
		return [3]float64{}, err
	}
	return [3]float64{a.Load1, a.Load5, a.Load15}, nil
}
