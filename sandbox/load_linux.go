package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// osFiles is this system's files, as the search for a program's load folders reads them.
type osFiles struct{}

func (osFiles) resolve(name string) (string, error) { return filepath.EvalSymlinks(name) }

// object reads the file at name, which is opened without waiting, so a FIFO put where a library is
// looked for holds nothing up, and read only when it is a regular file.
func (osFiles) object(name string) (o object, err error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the program its caller names, and the files its own search lists name, are what this reads
	if err != nil {
		return object{}, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return object{}, errors.Join(errors.New("it is not a regular file"), err)
	}
	return readObject(f, uint64(os.Getpagesize())) //nolint:gosec // G115: a page size is above 0
}

// loadHere is the folders prog needs to load on this system: its loader's and its libraries', by
// the loader's own search.
func loadHere(prog string) ([]string, error) { return loadFolders(osFiles{}, builtArch, prog) }
