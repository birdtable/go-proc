//go:build !windows

package gittest

// linksAreFiles is whether TestGitsPorcelainOfOneSmallRepositoryIsTheBytesItsSystemHolds writes its
// link as a file: off Windows the link is made.
const linksAreFiles = false

// heldOwn is the expectation of this system's own, beside porcelain.txt: Linux and macOS print the
// same bytes, so they read porcelain.txt itself.
const heldOwn = "porcelain.txt"
