// Package execsites holds go-proc to the execs it has: its tests list each reference to syscall.Exec,
// to x/sys/unix's Exec and to the sandbox's spawnExec (posix_spawn with POSIX_SPAWN_SETEXEC, the macOS
// block's exec) in the module's non-test source, by file and enclosing function, and want exactly the
// shim's two and the sandbox block's two. They also list each reference to the sandbox's spawnAddr,
// posix_spawn's trampoline, outside sandbox/spawn_darwin.go, and want none: rawSyscall6 calls it
// from any file of the package without spawnExec. Lint bans both Execs, but a //nolint line passes
// lint, and no lint row names spawnExec or spawnAddr, so only a reading of the source holds that no
// other exec is added.
//
// A file could also make an exec of its own: import posix_spawn or execve by //go:cgo_import_dynamic,
// link syscall's rawSyscall6 by //go:linkname, and call it. So the tests also list each reference to
// syscall6 and rawSyscall6, and each //go:linkname and //go:cgo_import_dynamic directive, in every Go
// file of the module, test files included, outside sandbox/spawn_darwin.go, by file and line, and
// want none. The compiler refuses the other //go:cgo_ directives outside cgo's own files.
//
// Nor may a file call libc through cgo, or the kernel through code no row of Go reads. The tests
// list each import of "C" in every Go file of the module, test files included, by file and line,
// and want none. They also list every file outside testdata that the go command builds into a
// package but is not Go, by go/build's own list of extensions, whatever system or tag it builds for:
// C, C++, Objective-C, Fortran, assembly, headers, SWIG and .syso objects. They want exactly the macOS
// block's two trampolines, sandbox/spawn_darwin_amd64.s and sandbox/spawn_darwin_arm64.s. So cgo
// comes in by neither import "C" nor SWIG, and assembly only by those two files. The module has no
// cgo, and a later need for it, or for another such file, is a ruling, not an entry here.
//
// Not held here, and held by review: a raw execve, Syscall(SYS_EXECVE, ...), which the raw-syscall
// lint row keeps to a file built for one system; and what the two trampolines say, as the tests
// read no assembly.
// It has no code of its own; its tests read the module's source.
package execsites
