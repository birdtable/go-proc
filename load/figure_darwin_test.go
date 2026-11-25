package load

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// systemFigure is what `sysctl -n vm.loadavg` prints, "{ 29.81 13.95 17.43 }", read by the test
// through proc: the package itself runs no program.
func systemFigure(t *testing.T) [3]float64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	res, err := proc.Run(ctx, proc.Cmd{Path: "/usr/sbin/sysctl", Args: []string{"-n", "vm.loadavg"}, Timeout: 30 * time.Second, MaxOutput: 4096, Drain: 10 * time.Second})
	if err != nil || res.Code != 0 {
		t.Fatalf("sysctl -n vm.loadavg: code %d, %v\n%s", res.Code, err, res.Stderr)
	}
	f := strings.Fields(string(res.Stdout))
	if len(f) != 5 || f[0] != "{" || f[4] != "}" {
		t.Fatalf("sysctl -n vm.loadavg printed %q, not { a b c }", res.Stdout)
	}
	var sys [3]float64
	for i := range sys {
		if sys[i], err = strconv.ParseFloat(f[i+1], 64); err != nil {
			t.Fatalf("sysctl -n vm.loadavg printed %q: %v", res.Stdout, err)
		}
	}
	return sys
}

// loadavg is a struct loadavg as the kernel lays it out: three uint32 averages, four bytes of
// padding, and the int64 scale.
func loadavg(one, five, fifteen uint32, scale uint64, size int) []byte {
	b := make([]byte, max(size, loadavgSize))
	binary.NativeEndian.PutUint32(b[0:], one)
	binary.NativeEndian.PutUint32(b[4:], five)
	binary.NativeEndian.PutUint32(b[8:], fifteen)
	binary.NativeEndian.PutUint64(b[16:], scale)
	return b[:size]
}

// TestDecodeReadsEachAverageByTheStructsOwnScale: decode divides each fixed-point average by the
// scale the struct carries, whatever it is, and refuses a struct of the wrong size or a scale that
// is zero or negative.
//
// What it costs when this fails: on a Mac whose kernel scales by another figure than 2048, every
// average is off by the same factor, and a caller's limit of 8 acts at 4 or at 16.
func TestDecodeReadsEachAverageByTheStructsOwnScale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		b    []byte
		want [3]float64
		ok   bool
	}{
		{"by 2048, the kernel's FSCALE", loadavg(3*2048, 1024, 50*2048+512, 2048, 24), [3]float64{3, 0.5, 50.25}, true},
		{"by 1000", loadavg(2500, 0, 125, 1000, 24), [3]float64{2.5, 0, 0.125}, true},
		{"20 bytes", loadavg(2048, 2048, 2048, 2048, 20), [3]float64{}, false},
		{"32 bytes", loadavg(2048, 2048, 2048, 2048, 32), [3]float64{}, false},
		{"a scale of zero", loadavg(2048, 2048, 2048, 0, 24), [3]float64{}, false},
		{"a negative scale", loadavg(2048, 2048, 2048, 1<<63, 24), [3]float64{}, false},
	} {
		got, err := decode(tc.b)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("%s: decode = %v, %v; want %v, ok %v", tc.name, got, err, tc.want, tc.ok)
		}
	}
}
