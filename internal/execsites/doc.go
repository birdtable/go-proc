// Package execsites holds go-proc to the execs it has: its tests list each reference to syscall.Exec,
// to x/sys/unix's Exec and to the sandbox's spawnExec (posix_spawn with POSIX_SPAWN_SETEXEC, the macOS
// block's exec) in the module's non-test source, by file and enclosing function, and want exactly the
// shim's two and the sandbox block's two. They also list each reference to the sandbox's spawnAddr,
// posix_spawn's trampoline, outside sandbox/spawn_darwin.go, and want none: rawSyscall6 calls it
// from any file of the package without spawnExec. Lint bans both Execs, but a //nolint line passes
// lint, and no lint row names spawnExec or spawnAddr, so only a reading of the source holds that no
// other exec is added. A raw execve, Syscall(SYS_EXECVE, ...), is not held here: the raw-syscall
// lint row keeps it to a file built for one system, and review holds the rest. Nor is the assembly
// that sets spawnAddr, as the tests read only Go files; review holds it.
// It has no code of its own; its tests read the module's source.
package execsites
