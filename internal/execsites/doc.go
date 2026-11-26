// Package execsites holds go-proc to the execs it has: its tests list each reference to syscall.Exec
// and to x/sys/unix's Exec in the module's non-test source, by file and enclosing function, and want
// exactly the shim's two and the sandbox block's two. Lint bans both, but a //nolint line passes
// lint, so only a reading of the source holds that no other exec is added.
// It has no code of its own; its tests read the module's source.
package execsites
