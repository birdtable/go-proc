//go:build darwin

#include "textflag.h"

// Each trampoline jumps to the libSystem function spawn_darwin.go imports, and its address word is
// what spawnExec hands syscall6, or rawSyscall6 for the spawn, as golang.org/x/sys/unix does on darwin.

TEXT libc_posix_spawn_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn(SB)
GLOBL	·spawnAddr(SB), RODATA, $8
DATA	·spawnAddr(SB)/8, $libc_posix_spawn_trampoline<>(SB)

TEXT libc_posix_spawnattr_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_init(SB)
GLOBL	·attrInitAddr(SB), RODATA, $8
DATA	·attrInitAddr(SB)/8, $libc_posix_spawnattr_init_trampoline<>(SB)

TEXT libc_posix_spawnattr_setflags_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setflags(SB)
GLOBL	·attrFlagsAddr(SB), RODATA, $8
DATA	·attrFlagsAddr(SB)/8, $libc_posix_spawnattr_setflags_trampoline<>(SB)

TEXT libc_posix_spawnattr_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_destroy(SB)
GLOBL	·attrDestroyAddr(SB), RODATA, $8
DATA	·attrDestroyAddr(SB)/8, $libc_posix_spawnattr_destroy_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_init(SB)
GLOBL	·actionsInitAddr(SB), RODATA, $8
DATA	·actionsInitAddr(SB)/8, $libc_posix_spawn_file_actions_init_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_addinherit_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_addinherit_np(SB)
GLOBL	·actionsInheritAddr(SB), RODATA, $8
DATA	·actionsInheritAddr(SB)/8, $libc_posix_spawn_file_actions_addinherit_np_trampoline<>(SB)

TEXT libc_posix_spawn_file_actions_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn_file_actions_destroy(SB)
GLOBL	·actionsDestroyAddr(SB), RODATA, $8
DATA	·actionsDestroyAddr(SB)/8, $libc_posix_spawn_file_actions_destroy_trampoline<>(SB)
