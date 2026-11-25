//go:build !linux && !windows

package proc

// limitsHere is which limits this system holds: none.
const limitsHere = limNone
