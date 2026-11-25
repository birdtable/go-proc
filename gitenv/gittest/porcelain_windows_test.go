package gittest

// linksAreFiles is whether TestGitsPorcelainOfOneSmallRepositoryIsTheBytesItsSystemHolds writes its
// link as a file: on Windows it does, as git checks a link out where core.symlinks is false.
const linksAreFiles = true

// heldOwn is Windows' own expectation, beside porcelain.txt: the lines where Windows differs, each
// named with its cause in the test's doc.
const heldOwn = "porcelain_windows.txt"
