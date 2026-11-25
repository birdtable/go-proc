//go:build dragonfly || freebsd || netbsd || openbsd

package proc

// maxrssUnit is ru_maxrss's unit in bytes: the BSDs give KiB.
const maxrssUnit = 1024
