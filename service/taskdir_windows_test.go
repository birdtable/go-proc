package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the rows read a folder's owner and DACL, and make a junction, by the calls Windows has for them
)

// tokenUser is this process's user.
func tokenUser(t *testing.T) *windows.SID {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid
}

// sddl is the descriptor s gives, with {u} in it the user's SID.
func sddl(t *testing.T, s string, user *windows.SID) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(strings.ReplaceAll(s, "{u}", user.String()))
	if err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return sd
}

// soleACE fails the test unless sd's owner is user and its DACL is protected from its parent's and
// one ACE that allows user full access.
func soleACE(t *testing.T, what string, sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) {
	t.Helper()
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		t.Fatalf("%s has no DACL (%v): %s", what, err, sd)
	}
	if acl.AceCount != 1 {
		t.Errorf("%s grants %d ACEs, want the user's alone: %s", what, acl.AceCount, sd)
		return
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // G103: the ACE's SID starts at SidStart, as the struct Windows defines it
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask != fileAllAccess || !sid.Equals(user) {
		t.Errorf("%s: its one ACE is type %d, mask %#x, for %s, want an allow of %#x for the user %s: %s", what, ace.Header.AceType, ace.Mask, sid, fileAllAccess, user, sd)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		t.Errorf("%s: its owner is %v (%v), want the user %s: %s", what, owner, err, user, sd)
	}
	if ctl, _, err := sd.Control(); err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s: its DACL is not protected from the temp folder's (control %#x, %v): %s", what, ctl, err, sd)
	}
}

// soleACEAt is soleACE for the descriptor Windows holds for path, read by a handle that shares all,
// as the install's own handle on the folder lets no other opener but one that shares delete.
func soleACEAt(t *testing.T, what, path string) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("%s %s: %v", what, path, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("%s %s: %v", what, path, err)
	}
	soleACE(t, what+" "+path, sd, tokenUser(t))
}

// held fails the test unless renaming dir fails with a sharing violation, as it does while the
// install holds the folder open; a rename that goes through is undone, so the install goes on.
func held(t *testing.T, when, dir string) {
	t.Helper()
	moved := dir + "-moved"
	err := os.Rename(dir, moved)
	if err == nil {
		t.Errorf("%s: the task's folder %s could be renamed away, so another account with delete child on the temp folder could put its own at the path", when, dir)
		if err := os.Rename(moved, dir); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Errorf("%s: renaming the task's folder %s gave %v, want a sharing violation", when, dir, err)
	}
}

// sameDir fails the test unless a and b are one folder.
func sameDir(t *testing.T, what, a, b string) {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !os.SameFile(fa, fb) {
		t.Errorf("%s: %s is not the folder %s", what, a, b)
	}
}

// gone fails the test unless nothing is at dir.
func gone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the task's folder %s is left after the install (%v)", dir, err)
	}
}

// junction makes link a junction to the folder target, as mklink /J does: a folder with a mount
// point's reparse data, which Go has no call for.
func junction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Mkdir(link, 0o700); err != nil {
		t.Fatal(err)
	}
	sub, print := utf16.Encode([]rune(`\??\`+target)), utf16.Encode([]rune(target))
	names := append(append(append(sub, 0), print...), 0)
	// REPARSE_DATA_BUFFER of a mount point: its tag, its data's length and a reserved 0, then the
	// substitute and print names' offsets and lengths in bytes, then the names, each ended by a NUL.
	b := binary.LittleEndian.AppendUint32(nil, windows.IO_REPARSE_TAG_MOUNT_POINT)
	for _, v := range []int{8 + 2*len(names), 0, 0, 2 * len(sub), 2 * (len(sub) + 1), 2 * len(print)} {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	for _, c := range names {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	p, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &b[0], uint32(len(b)), nil, 0, &n, nil); err != nil {
		t.Fatalf("%s as a junction to %s: %v", link, target, err)
	}
}

// TestATaskFolderDescriptorIsTheUsersAlone: the descriptor a task's folder and its task.xml are
// made with names the user as its owner, not the owner the token would give, and has a protected
// DACL of one ACE that allows that user full access. It reads the descriptor itself, so it holds
// whatever the token: an unelevated token's default owner is the user already, and the folder row
// alone would not tell.
//
// What it costs when this fails: under an elevated token the folder's owner is the Administrators
// group, and any member of it may rewrite the folder's DACL and so task.xml.
func TestATaskFolderDescriptorIsTheUsersAlone(t *testing.T) {
	t.Parallel()
	user := tokenUser(t)
	sd, err := taskDirSD(user.String())
	if err != nil {
		t.Fatal(err)
	}
	soleACE(t, "the descriptor makeTaskFolder makes the folder with", sd, user)
}

// TestOnlyTheUsersOwnDescriptorReadsBackAsATaskFolders: the check a made folder's descriptor is
// read back by passes the one taskDirSD makes, and no descriptor that differs from it in its owner,
// its protection, or its one ACE's type, mask or SID, nor one with no owner, no ACE or no DACL.
//
// What it costs when this fails: a folder whose descriptor another account's grant reached, as on a
// file system that keeps no ACLs, is written in, and another account rewrites task.xml before
// schtasks reads it.
func TestOnlyTheUsersOwnDescriptorReadsBackAsATaskFolders(t *testing.T) {
	t.Parallel()
	user := tokenUser(t)
	for _, tc := range []struct {
		sddl string
		want bool
	}{
		{"O:{u}D:P(A;OICI;FA;;;{u})", true},
		{"O:{u}D:(A;OICI;FA;;;{u})", false},
		{"O:BAD:P(A;OICI;FA;;;{u})", false},
		{"D:P(A;OICI;FA;;;{u})", false},
		{"O:{u}D:P(A;OICI;FA;;;{u})(A;OICI;FA;;;BU)", false},
		{"O:{u}D:P(A;OICI;FA;;;BU)", false},
		{"O:{u}D:P(A;OICI;FR;;;{u})", false},
		{"O:{u}D:P(D;OICI;FA;;;{u})", false},
		{"O:{u}D:P", false},
		{"O:{u}D:NO_ACCESS_CONTROL", false},
	} {
		if got := soleUser(sddl(t, tc.sddl, user), user); got != tc.want {
			t.Errorf("%s reads back as the user's alone: %t, want %t", tc.sddl, got, tc.want)
		}
	}
}

// TestAWindowsTaskFolderIsTheUsersAlone: on Windows the folder an install writes a task's XML in is
// owned by this process's user, with a DACL protected from the temp folder's that allows that user
// alone, from before the XML is written in it; task.xml, as schtasks is given it, is too, its owner
// pinned as the folder's is; and the folder cannot be renamed, before the write, while schtasks runs
// or as it is removed, as the install holds it open without sharing delete until it is gone. The
// temp folder is one of the test's own, granting the Users group inheritable modify, as a shared
// TEMP does.
//
// What it costs when this fails: another account that may modify the temp folder rewrites, or
// removes and makes again, task.xml between its write and schtasks' read; or one with delete child
// on it renames the folder away and puts its own at the path, before schtasks reads it or before it
// is removed by its path; and schtasks takes a task no check of the package read, or the install
// removes what the other account put there.
func TestAWindowsTaskFolderIsTheUsersAlone(t *testing.T) {
	// serial: it sets TMP and TEMP, which os.TempDir reads for the whole process
	root := t.TempDir()
	dacl, _, err := sddl(t, "D:(A;OICI;0x1301bf;;;BU)", tokenUser(t)).DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	p := plans(t.TempDir())["windows"]
	sys := &system{goos: "windows"}
	var given, removing string
	run := func(ctx context.Context, argv []string) ([]byte, int, error) {
		if argv[1] == "/create" {
			given = argv[5]
			soleACEAt(t, "task.xml as schtasks is given it", given)
			held(t, "as schtasks is given task.xml", filepath.Dir(given))
		}
		return sys.run(ctx, argv)
	}
	h := hostOf("windows")
	staged := ""
	h.staged = func(dir string) {
		staged = dir
		sameDir(t, "the task's folder's parent", filepath.Dir(dir), root)
		soleACEAt(t, "the task's folder before the write", dir)
		held(t, "before the write", dir)
	}
	h.task.removing = func(dir string) {
		removing = dir
		held(t, "as the folder is removed", dir)
	}
	if err := install(t.Context(), h, p, run); err != nil {
		t.Fatalf("the install gave %v, want nil", err)
	}
	if staged == "" || given == "" || removing != staged {
		t.Fatalf("the install made no folder (%q), gave schtasks no file (%q) or removed another folder (%q)", staged, given, removing)
	}
	gone(t, staged)
}

// TestAWindowsTaskFolderIsNotReachedThroughTheTempFolderAgain: when the temp folder is a junction,
// schtasks is given task.xml by the path of the folder the install made and holds, not by a path
// through the junction, so a junction pointed elsewhere once the folder is made, at a folder of the
// same name with a task.xml of its own, does not give schtasks that file.
//
// What it costs when this fails: another account that may change a junction the temp folder is, as
// one it made or one in a folder it may write, points it at its own folder between the folder's
// making and schtasks' read, and schtasks takes that account's task.
func TestAWindowsTaskFolderIsNotReachedThroughTheTempFolderAgain(t *testing.T) {
	// serial: it sets TMP and TEMP, which os.TempDir reads for the whole process
	base := t.TempDir()
	made, other, link := filepath.Join(base, "made"), filepath.Join(base, "other"), filepath.Join(base, "temp")
	for _, dir := range []string{made, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	junction(t, link, made)
	t.Setenv("TMP", link)
	t.Setenv("TEMP", link)
	p := plans(t.TempDir())["windows"]
	sys := &system{goos: "windows"}
	var given, name string
	var body []byte
	run := func(ctx context.Context, argv []string) ([]byte, int, error) {
		if argv[1] == "/create" {
			given = argv[5]
			sameDir(t, "the folder of task.xml as schtasks is given it", filepath.Dir(given), filepath.Join(made, name))
			var err error
			if body, err = os.ReadFile(given); err != nil {
				t.Errorf("task.xml as schtasks is given it: %v", err)
			}
		}
		return sys.run(ctx, argv)
	}
	h := hostOf("windows")
	h.staged = func(dir string) {
		name = filepath.Base(dir)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		junction(t, link, other)
		if err := os.Mkdir(filepath.Join(other, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(other, name, "task.xml"), []byte("another account's task"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := install(t.Context(), h, p, run); err != nil {
		t.Fatalf("the install gave %v, want nil", err)
	}
	if given == "" {
		t.Fatal("the install gave schtasks no file")
	}
	if !bytes.Equal(body, p.Files[0].Bytes()) {
		t.Errorf("schtasks was given %q, want the task's own XML", body)
	}
	gone(t, filepath.Join(made, name))
}

// TestAWindowsInstallRefusesATaskFolderThatDoesNotReadBackAsMade: when the folder's descriptor, read
// back by its handle, is not the user's alone, as a file system that keeps no ACLs gives, the
// install is refused before anything is written in it or schtasks runs, and the folder is removed.
//
// What it costs when this fails: task.xml is written in a folder another account may reach, as on
// a temp folder on FAT or exFAT, and that account rewrites it before schtasks reads it.
func TestAWindowsInstallRefusesATaskFolderThatDoesNotReadBackAsMade(t *testing.T) {
	t.Parallel()
	user := tokenUser(t)
	var removing string
	ran := 0
	run := func(context.Context, []string) ([]byte, int, error) { ran++; return nil, 0, nil }
	h := hostOf("windows")
	h.task.folderSD = func() (*windows.SECURITY_DESCRIPTOR, error) {
		return sddl(t, "O:{u}D:(A;OICI;FA;;;{u})(A;OICI;FA;;;WD)", user), nil
	}
	h.task.removing = func(dir string) { removing = dir }
	h.staged = func(dir string) { t.Errorf("the install went on to write in %s", dir) }
	err := install(t.Context(), h, plans(t.TempDir())["windows"], run)
	if !errors.Is(err, errTaskFolderOpen) {
		t.Errorf("the install gave %v, want %v", err, errTaskFolderOpen)
	}
	if ran != 0 {
		t.Errorf("schtasks ran %d times, want none", ran)
	}
	if removing == "" {
		t.Fatal("the refused folder was not removed")
	}
	gone(t, removing)
}
