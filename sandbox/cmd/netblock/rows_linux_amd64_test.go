//go:build linux && amd64

package main

// sysProcessVMWritev is process_vm_writev on amd64, from the kernel's
// syscall_64.tbl: package syscall does not name it there.
const sysProcessVMWritev = 311

// sysSendmmsg is sendmmsg on amd64, from the kernel's syscall_64.tbl:
// package syscall does not name it there.
const sysSendmmsg = 307
