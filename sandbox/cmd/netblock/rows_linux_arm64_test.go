//go:build linux && arm64

package main

import "syscall"

// sysProcessVMWritev is process_vm_writev on arm64.
const sysProcessVMWritev = syscall.SYS_PROCESS_VM_WRITEV

// sysSendmmsg is sendmmsg on arm64.
const sysSendmmsg = syscall.SYS_SENDMMSG
