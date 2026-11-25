// Package load reads the system's load average: the one, five and fifteen minute averages the
// kernel keeps of the threads that are running or waiting to run, with the number of CPUs this
// process can use beside them. It reads and returns the figures; it sets no threshold and says
// nothing of what a load means, which is the caller's to decide.
//
// On macOS Read reads the vm.loadavg sysctl itself, not by gopsutil, whose macOS build's purego takes
// over the runtime's cgo hooks when cgo is off. On Linux it is built on gopsutil's load.Avg, which
// reads /proc/loadavg, its folder pinned whatever HOST_PROC says, and sysinfo(2) when that file
// fails. Windows keeps no load average (gopsutil makes one up from a processor queue; load takes
// none of it), so there, on every other system, and when the source cannot be read or parsed, Read
// returns an error that matches ErrNoAverage and errors.ErrUnsupported, never a zero Reading in its
// place. It starts no program.
package load

import (
	"errors"
	"fmt"
	"math"
	"runtime"
)

// ErrNoAverage is the error of a Read that has no load average to give: the system keeps none
// that this package reads, or its source could not be read or parsed. Read wraps the cause with
// it, and it wraps errors.ErrUnsupported.
var ErrNoAverage = fmt.Errorf("load: no load average: %w", errors.ErrUnsupported)

// errNoAverage is what Read wraps: ErrNoAverage as built, so a value a caller or a dependency
// stores in it later, nil included, changes what a match finds and never what Read refuses.
var errNoAverage = ErrNoAverage

// Reading is the system's load average when Read was called.
type Reading struct {
	// One, Five and Fifteen are the kernel's averages over one, five and fifteen minutes of the
	// threads running or waiting to run, as it keeps them: macOS counts the threads waiting on
	// nothing but a CPU, Linux those in uninterruptible sleep too, such as on a disk.
	One, Five, Fifteen float64
	// CPUs is runtime.NumCPU(): the CPUs this process can use when Read was called.
	CPUs int
}

// Read reads the system's load average. Each average it returns is finite and not below zero;
// a source that gives one that is not is an error, as is one it cannot read or parse, and every
// error matches ErrNoAverage.
func Read() (Reading, error) {
	avg, err := read()
	if err == nil {
		err = check(avg)
	}
	if err != nil {
		return Reading{}, noAverage(err)
	}
	return Reading{One: avg[0], Five: avg[1], Fifteen: avg[2], CPUs: runtime.NumCPU()}, nil
}

// noAverage is Read's refusal for err, why it has no average: it matches ErrNoAverage as built and err.
func noAverage(err error) error { return fmt.Errorf("%w: %w", errNoAverage, err) }

// check refuses an average that is not a number, is infinite or is below zero, which no kernel
// keeps, so a source that gives one is not read as it should be.
func check(avg [3]float64) error {
	for _, v := range avg {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("the source gives an average of %v", v)
		}
	}
	return nil
}
