//go:build race

package proc

// raceMemory is the memory the race detector adds to a helper a row holds to treeLimit, so a row
// that holds a tree to treeLimit leaves it the margin it leaves it without -race.
// Measured by the peak resident set of this package's hog helper, built with and without -race, on
// Linux amd64: 9 MiB plain and 26 MiB under -race idle, 26 against 55 MiB with a hog of 16 MiB and 57
// against 120 MiB with a hog of 48 MiB: the detector's shadow memory is about twice what the helper
// allocates, and 17 MiB more besides. A cgroup counts the binary's file cache and the kernel's own
// pages too, which that measure leaves out, and a Contained helper's 16 MiB hog read a Peak of
// 64 MiB under -race: so this is the 29 MiB measured for a hog of 16 MiB,
// the largest a row holds under treeLimit, with 19 MiB over it for those. A hog of 128 MiB, which a
// row means to pass the limit, is far past it with or without this.
//
// A Contained run is also charged the shim's last image, the caller's binary, until the kernel has
// freed it after the program's exec: on linux/arm64 under -race 45 to 48 MiB of it at the program's
// start, and gone a second later. So this is 64 MiB, the 48 rounded up.
const raceMemory = 64 << 20
