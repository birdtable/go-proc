// Package secondconsumer is go-proc as a second business would use it: its
// tests run each package's main path through the exported API alone, with values deliberately unlike
// go-proc's own, and fail wherever a package fills in a value of ours in place of the caller's. It
// has no code of its own; it runs in the commit tier, so a pull request that puts a value of ours
// back where the caller's belongs goes red there.
package secondconsumer
