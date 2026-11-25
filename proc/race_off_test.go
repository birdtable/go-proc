//go:build !race

package proc

// raceMemory is the memory the race detector adds to a helper a row holds to treeLimit: none in a
// build without it. race_on_test.go has the measure.
const raceMemory = 0
