//go:build windows

package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a folder made with a security descriptor of its own, held, written, read back and removed by its handle, is x/sys's calls alone
)

const (
	// taskDirTries is how many names makeTaskFolder tries before it gives up, each 8 random bytes:
	// a second try is met only when something of that name is already there.
	taskDirTries = 8
	// fileAddSubdirectory is FILE_ADD_SUBDIRECTORY of winnt.h, the right to make a folder in a
	// folder, which x/sys does not name.
	fileAddSubdirectory = 0x4
	// fileAllAccess is FILE_ALL_ACCESS of winnt.h, the mask SDDL's FA names.
	fileAllAccess windows.ACCESS_MASK = 0x1F01FF
	shareAll                          = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	// pathLen is the longest path, in UTF-16 units, the folder's path is read into.
	pathLen = windows.MAX_LONG_PATH
)

// errTaskFolderOpen is the reason an install is refused when the folder it made does not read back
// as taskDirSD made it.
var errTaskFolderOpen = errors.New("the task's folder does not read back as it was made, the user its owner and a protected DACL that allows the user alone, as on a file system that keeps no ACLs, so another account could reach task.xml")

// taskSeams are nil but in a test of the package, and taken only under testing.Testing(): folderSD
// stands in for the descriptor read back from a made task folder, and removing runs as the folder
// is removed, while it is still held.
type taskSeams struct {
	folderSD func() (*windows.SECURITY_DESCRIPTOR, error)
	removing func(dir string)
}

// taskDirSD is the descriptor a task's folder and its task.xml are made with: its owner is sid,
// whatever owner the token would give (an elevated administrator's is the Administrators group), and
// its DACL, protected from the temp folder's, grants sid alone full access.
func taskDirSD(sid string) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
}

// taskFolder is the folder one task's XML is written in, held by a handle that shares no delete from
// its making to its removal, so no account may rename or remove it meanwhile.
type taskFolder struct {
	dir   string                       // its path as the handle names it, through no junction or link
	h     windows.Handle               // the folder, open for listing, its descriptor and its delete
	sd    *windows.SECURITY_DESCRIPTOR // taskDirSD for this process's user
	seams taskSeams
}

// makeTaskFolder makes a new folder, task- and 16 random hex digits, in the system's temp folder,
// with taskDirSD for this process's user, and holds it. The descriptor is the folder's from the call
// that makes it, so nothing of the temp folder's grants is on it; it is read back by the handle, and
// a folder that does not keep it is removed and refused (errTaskFolderOpen). Its path is the one the
// handle names, so a temp folder that is a junction, which its own grants may let another account
// point elsewhere, is not on the path schtasks reads.
func makeTaskFolder(hst host) (f *taskFolder, err error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("this process's user could not be read: %w", err)
	}
	sd, err := taskDirSD(u.User.Sid.String())
	if err != nil {
		return nil, err
	}
	root := os.TempDir()
	rp, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	rh, err := windows.CreateFile(rp, windows.FILE_TRAVERSE|fileAddSubdirectory|windows.SYNCHRONIZE, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	defer func() {
		if cerr := windows.CloseHandle(rh); cerr != nil && err == nil {
			err, f = errors.Join(cerr, f.remove()), nil
		}
	}()
	for range taskDirTries {
		var b [8]byte
		_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error: it ends the program when it cannot read
		name := "task-" + hex.EncodeToString(b[:])
		h, err := ntCreate(rh, name, sd, ntOpen{
			access: windows.FILE_LIST_DIRECTORY | windows.DELETE | windows.READ_CONTROL | windows.SYNCHRONIZE,
			share:  windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE, disposition: windows.FILE_CREATE, options: windows.FILE_DIRECTORY_FILE,
		})
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "mkdir", Path: filepath.Join(root, name), Err: err}
		}
		f = &taskFolder{h: h, sd: sd}
		if testing.Testing() {
			f.seams = hst.task
		}
		if err := f.check(u.User.Sid); err != nil {
			return nil, errors.Join(err, f.remove())
		}
		return f, nil
	}
	return nil, fmt.Errorf("%d names in %s were each taken", taskDirTries, root)
}

// check reads the folder's path and its descriptor back by its handle. The path keeps its \\?\, so
// no Win32 rule of names, as a trailing dot or space dropped, turns it to another folder.
func (f *taskFolder) check(user *windows.SID) error {
	buf := make([]uint16, pathLen)
	n, err := windows.GetFinalPathNameByHandle(f.h, &buf[0], pathLen, 0)
	if err != nil || n >= pathLen {
		return errors.Join(errors.New("the task's folder's path could not be read from its handle"), err)
	}
	f.dir = windows.UTF16ToString(buf[:n])
	sd, err := windows.GetSecurityInfo(f.h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if f.seams.folderSD != nil {
		sd, err = f.seams.folderSD()
	}
	if err != nil {
		return err
	}
	if !soleUser(sd, user) {
		return errTaskFolderOpen
	}
	return nil
}

// soleUser is whether sd's owner is user and its DACL is protected and one ACE that allows user full
// access.
func soleUser(sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) bool {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return false
	}
	if ctl, _, err := sd.Control(); err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		return false
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if windows.GetAce(acl, 0, &ace) != nil {
		return false
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)) //nolint:gosec // G103: the ACE's SID starts at SidStart, as the struct Windows defines it
	return ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE && ace.Mask == fileAllAccess && sid.Equals(user)
}

// write makes the file name in the folder, by its handle and with the folder's descriptor, so its
// owner is pinned as the folder's is, and writes b to it: something of that name already there
// fails it (fs.ErrExist).
func (f *taskFolder) write(name string, b []byte) error {
	fh, err := ntCreate(f.h, name, f.sd, ntOpen{
		access: windows.FILE_GENERIC_WRITE, share: windows.FILE_SHARE_READ, disposition: windows.FILE_CREATE, options: windows.FILE_NON_DIRECTORY_FILE,
	})
	if err != nil {
		return err
	}
	var n uint32
	err = windows.WriteFile(fh, b, &n, nil)
	if err == nil && int(n) != len(b) {
		err = io.ErrShortWrite
	}
	return errors.Join(err, windows.CloseHandle(fh))
}

// remove deletes task.xml and the folder by its handle, then lets the folder go, so no path is
// resolved after the hold ends. Anything else in the folder, which only the user's own programs could
// put there, fails it and leaves the folder.
func (f *taskFolder) remove() error {
	fh, err := ntCreate(f.h, "task.xml", nil, ntOpen{
		access: windows.DELETE | windows.SYNCHRONIZE, share: shareAll, disposition: windows.FILE_OPEN, options: windows.FILE_OPEN_REPARSE_POINT,
	})
	if err == nil {
		err = errors.Join(dispose(fh), windows.CloseHandle(fh))
	} else if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		err = nil
	}
	if f.seams.removing != nil {
		f.seams.removing(f.dir)
	}
	return errors.Join(err, dispose(f.h), windows.CloseHandle(f.h))
}

// dispose marks what h holds for deletion, its name gone at once (POSIX semantics) though another
// handle, as a scanner's, has it open; a file system without them deletes it at its last close.
func dispose(h windows.Handle) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), 4) //nolint:gosec // G103: FILE_DISPOSITION_INFO_EX is its one ULONG of flags
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		deleteFile := uint8(1) // FILE_DISPOSITION_INFO, whose one field is the BOOLEAN DeleteFile
		err = windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &deleteFile, 1)
	}
	return err
}

// ntOpen is how ntCreate opens or makes a name: NtCreateFile's access, share, disposition and
// options.
type ntOpen struct{ access, share, disposition, options uint32 }

// ntCreate opens or makes name in the folder root holds, by NtCreateFile relative to that handle, so
// no path above it is read again; sd, when not nil, is what a made object takes. An NTSTATUS comes
// back as the Win32 error it maps to, so a name already there is fs.ErrExist.
func ntCreate(root windows.Handle, name string, sd *windows.SECURITY_DESCRIPTOR, o ntOpen) (windows.Handle, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: root, ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, o.access, &oa, &iosb, nil, 0, o.share, o.disposition, o.options|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	runtime.KeepAlive(sd)
	runtime.KeepAlive(n)
	if st, ok := errors.AsType[windows.NTStatus](err); ok {
		return 0, st.Errno()
	}
	return h, err
}
