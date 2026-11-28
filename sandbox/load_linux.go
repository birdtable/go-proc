package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// osFiles is this system's files, as the search for a program's load folders reads them.
type osFiles struct{}

// resolve is name with every link in it resolved, and whether its stat, which follows links as glibc's
// does, finds a folder, once the kernel has walked name as written, as the loader's open walks it: a
// folder on the way the caller cannot search, a path past the kernel's length limit and a walk past its
// link limit are the kernel's error, never the path EvalSymlinks reads by the text, which asks none of
// them. A path the kernel walks and EvalSymlinks then cannot, whose real path is past the length limit
// though each link's text is short, is errUntold: the loader opens the file there.
func (osFiles) resolve(name string) (string, bool, error) {
	st, err := os.Stat(name)
	if err != nil {
		return "", false, err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", errUntold, err)
	}
	return resolved, st.IsDir(), nil
}

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

// head is the first maxLoader bytes of the file at name, opened without waiting, as object opens
// it, and read only when it is a regular file.
func (osFiles) head(name string) (b []byte, err error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the loader the program names, by its real path, is what this reads
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.Join(errors.New("it is not a regular file"), err)
	}
	return io.ReadAll(io.LimitReader(f, maxLoader))
}

// loadHere is the folders prog, whose real file is real, needs to load on this system: its
// loader's first, then its libraries', by the loader's own search from that file, each library's
// beneath a folder of held, the caller's, or a built-in folder of the loader.
func loadHere(prog, real string, held []string) ([]string, error) {
	return loadFoldersOf(osFiles{}, builtArch, prog, real, held)
}
