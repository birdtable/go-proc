//go:build !darwin && !linux

package load

import "errors"

// read refuses: Windows keeps no load average, and this package reads none on any other system.
func read() ([3]float64, error) {
	return [3]float64{}, errors.New("this system keeps no load average that this package reads")
}
