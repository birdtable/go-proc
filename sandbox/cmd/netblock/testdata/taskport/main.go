//go:build darwin

// Command taskport is the netblock proof's macOS task-port rows, built with cgo:
//   - hold is the target: it waits until its stdin ends;
//   - take PID is the caller: it asks for PID's control port, task_for_pid, and says ok, refused
//     (KERN_FAILURE) or other, and "other no answer" when the system has not answered in 10s;
//   - name PID is the caller too: it asks for PID's name port, task_name_for_pid, which taskgated
//     is not asked for, and says the same words;
//   - asks says whether the system would give this session's processes a task port with no person
//     to approve it: allowed, "needs a person", denied, or other. It asks the rights taskgated asks,
//     without letting the system show a dialog.
//
// The test signs a copy of it with get-task-allow for the target, and runs a copy signed with no
// entitlement as the caller.
package main

/*
#cgo LDFLAGS: -framework Security
#include <mach/mach.h>
#include <mach/mach_traps.h>
#include <Security/Authorization.h>

static int control(int pid) {
	mach_port_t t = MACH_PORT_NULL;
	kern_return_t kr = task_for_pid(mach_task_self(), pid, &t);
	if (kr == KERN_SUCCESS) mach_port_deallocate(mach_task_self(), t);
	return kr;
}

static int name(int pid) {
	mach_port_t t = MACH_PORT_NULL;
	kern_return_t kr = task_name_for_pid(mach_task_self(), pid, &t);
	if (kr == KERN_SUCCESS) mach_port_deallocate(mach_task_self(), t);
	return kr;
}

static int asks(void) {
	AuthorizationRef ref;
	OSStatus st = AuthorizationCreate(NULL, kAuthorizationEmptyEnvironment, kAuthorizationFlagDefaults, &ref);
	if (st != errAuthorizationSuccess) return st;
	AuthorizationItem items[] = {
		{"system.privilege.taskport", 0, NULL, 0},
		{"system.privilege.taskport.debug", 0, NULL, 0},
	};
	AuthorizationRights rights = {2, items};
	st = AuthorizationCopyRights(ref, &rights, kAuthorizationEmptyEnvironment, kAuthorizationFlagExtendRights, NULL);
	AuthorizationFree(ref, kAuthorizationFlagDefaults);
	return st;
}
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

func main() {
	fmt.Println(say(os.Args[1:]))
}

func say(args []string) string {
	switch {
	case len(args) == 1 && args[0] == "hold":
		_, _ = io.Copy(io.Discard, os.Stdin)
		return "held"
	case len(args) == 2 && args[0] == "take":
		return port(args[1], func(pid C.int) C.int { return C.control(pid) })
	case len(args) == 2 && args[0] == "name":
		return port(args[1], func(pid C.int) C.int { return C.name(pid) })
	case len(args) == 1 && args[0] == "asks":
		switch st := C.asks(); st {
		case C.errAuthorizationSuccess:
			return "allowed"
		case C.errAuthorizationInteractionNotAllowed:
			return "needs a person"
		case C.errAuthorizationDenied:
			return "denied"
		default:
			return "other OSStatus " + strconv.Itoa(int(st))
		}
	}
	return fmt.Sprintf("other usage %q", args)
}

// port asks for a port of the process pid names by ask, and says ok, refused (KERN_FAILURE),
// other, or "other no answer" when the system has not answered in 10s.
func port(pid string, ask func(C.int) C.int) string {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return "other " + err.Error()
	}
	done := make(chan int, 1)
	go func() { done <- int(ask(C.int(n))) }()
	select {
	case kr := <-done:
		switch kr {
		case C.KERN_SUCCESS:
			return "ok"
		case C.KERN_FAILURE:
			return "refused"
		}
		return "other kern_return " + strconv.Itoa(kr)
	case <-time.After(10 * time.Second):
		return "other no answer"
	}
}
