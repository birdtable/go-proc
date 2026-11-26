// Package execsites holds go-proc to the execs it has: its tests list each reference to syscall.Exec,
// to x/sys/unix's Exec and to the sandbox's spawnExec (posix_spawn with POSIX_SPAWN_SETEXEC, the macOS
// block's exec) in the module's non-test source, by file and enclosing function, and want exactly the
// shim's two and the sandbox block's two. Lint bans both Execs, but a //nolint line passes lint, and
// no lint row names spawnExec, so only a reading of the source holds that no other exec is added.
// A raw execve, Syscall(SYS_EXECVE, ...), is not held here: the raw-syscall lint row keeps it to a
// file built for one system, and review holds the rest.
// It has no code of its own; its tests read the module's source.
package execsites
