//go:build !windows

package service

import (
	"errors"
	"os"
)

// taskSeams are Windows' alone: a test of the package plants none here.
type taskSeams struct{}

// taskFolder is the folder one task's XML is written in: os.MkdirTemp's, mode 0700. Only a test of
// the package installs a Windows plan here. It keeps the host's seams as Windows' does, and takes
// none.
type taskFolder struct {
	dir   string
	seams taskSeams
}

// makeTaskFolder makes a new folder in the system's temp folder.
func makeTaskFolder(h host) (*taskFolder, error) {
	dir, err := os.MkdirTemp("", "task-")
	return &taskFolder{dir: dir, seams: h.task}, err
}

// write writes b to a file it makes in the folder, mode 0600: a file already there fails it, and is
// left as it was.
func (f *taskFolder) write(name string, b []byte) (err error) {
	r, err := os.OpenRoot(f.dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	w, err := r.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return errors.Join(err, w.Close())
}

// remove removes the folder and what is in it.
func (f *taskFolder) remove() error { return os.RemoveAll(f.dir) }
