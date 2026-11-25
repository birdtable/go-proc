package load

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the load average is read by the vm.loadavg sysctl, which x/sys gives with no program run
)

// read reads the vm.loadavg sysctl by the system call, as sysctl(8) does, with no program run.
// It is not gopsutil's: that checks no length or scale and links purego's fakecgo.
func read() ([3]float64, error) {
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return [3]float64{}, fmt.Errorf("sysctl vm.loadavg: %w", err)
	}
	return decode(b)
}

// loadavgSize is the size of the kernel's struct loadavg on the 64-bit systems macOS runs on:
// three fixpt_t (uint32) averages, four bytes of padding, and a long fscale at offset 16.
const loadavgSize = 24

// decode reads a struct loadavg: each average is a fixed-point number whose 1.0 is fscale. A
// scale of zero, or one with the sign bit of the C long set, is refused.
func decode(b []byte) ([3]float64, error) {
	if len(b) != loadavgSize {
		return [3]float64{}, fmt.Errorf("vm.loadavg gives %d bytes, not the %d of struct loadavg", len(b), loadavgSize)
	}
	scale := binary.NativeEndian.Uint64(b[16:])
	if scale == 0 || scale>>63 != 0 {
		return [3]float64{}, fmt.Errorf("vm.loadavg gives a scale of %#x", scale)
	}
	var avg [3]float64
	for i := range avg {
		avg[i] = float64(binary.NativeEndian.Uint32(b[4*i:])) / float64(scale)
	}
	return avg, nil
}
