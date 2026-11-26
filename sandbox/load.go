package sandbox

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"slices"
	"strings"
)

// The load folders of a program are the folders it reads to start, beyond its own: its loader's
// and its libraries'. RunHolding grants them itself, read-only, from the program it is given, so a
// caller names none of them and cannot widen the list. The answer is the loader's own search as it
// goes behind the block, never the host's: the loader the program names, PT_INTERP, by its real
// path, then each library it needs, DT_NEEDED, and each those need, found in the folders the loader
// looks in with no cache, and answered by the folder of its real path. Each file is read as the
// kernel and the loaders read it: the first PT_INTERP, a second refused, and the dynamic entries
// at the PT_DYNAMIC segment's address in the file as its PT_LOAD segments map it, never by a
// section header. A file found for a library is one only where it is a shared object and no
// program; any other is refused, as glibc refuses it. A library whose real folder is the root or
// /usr itself is refused: either grant is the whole tree, or its system half, for bytes no caller
// judged, and no loader the search knows loads from there.
//
// glibc looks in the DT_RPATH of the file that needs the library, then of the file that needed that
// one, and so on up to the program, only when the file that needs it has no DT_RUNPATH, and never
// in the DT_RPATH of a file that has a DT_RUNPATH; then in the DT_RUNPATH of the file that needs
// it; then in its built-in folders. $ORIGIN, $LIB and $PLATFORM in a list stand for the folder of
// the file that holds it, the distribution's library folder and the machine. A file of another
// architecture is passed over. musl looks in the DT_RUNPATH, or with none the DT_RPATH, of the file
// that needs the library and of each above it, then in its built-in folders, knows $ORIGIN alone,
// and takes the first file it finds; the libraries it holds itself, libc and libm among them, are
// the loader.
//
// /etc/ld.so.cache, /etc/ld.so.conf and musl's /etc/ld-musl-<arch>.path are not read: behind the
// block the loader reads them only where the caller granted /etc. LD_LIBRARY_PATH, LD_PRELOAD and
// LD_AUDIT are not read either. Each reaches only a folder the caller granted itself, so a
// library found only by one of them is not found here, and the program is refused.
// glibc's glibc-hwcaps folders lie beneath a folder it searches, so the grant of that folder
// reaches them.
//
// The bounds of one search: maxLibraries files, maxLoadDepth files from the program to a library,
// maxPathList folders in one DT_RPATH or DT_RUNPATH, maxInterp bytes of a loader's name and
// maxDynamic bytes of a dynamic segment or of its strings.
const (
	maxLibraries = 1024
	maxLoadDepth = 64
	maxPathList  = 64
	maxInterp    = 4096
	maxDynamic   = 64 << 20
)

// object is what the search reads of one ELF file: its architecture, the loader it names ("" for
// none), the libraries it needs, its DT_RPATH and DT_RUNPATH, each nil when it has none, its
// DT_SONAME, whether a loader loads it as a library, and unread, why its dynamic segment could not
// be read when it has one.
type object struct {
	class          elf.Class
	machine        elf.Machine
	interp, soname string
	needed         []string
	rpath, runpath []string
	library        bool
	unread         error
}

// loadFiles is the files the search reads, a seam its rows run by on every system: resolve is a
// path with every link in it resolved, and object the ELF file at a path.
type loadFiles interface {
	resolve(name string) (string, error)
	object(name string) (object, error)
}

// readObject reads r as an ELF file, on a system whose pages are page bytes. Every way r is not
// one is an error, a panic of debug/elf among them, which is so a refusal and never a crash; a #!
// script is named as one, and so is a file that names more than one loader, where the kernel
// takes the first and the search would follow another.
func readObject(r io.ReaderAt, page uint64) (o object, err error) {
	defer func() {
		if p := recover(); p != nil {
			o, err = object{}, fmt.Errorf("reading it as an ELF file panicked: %v", p)
		}
	}()
	var magic [2]byte
	if _, err := r.ReadAt(magic[:], 0); err == nil && string(magic[:]) == "#!" {
		return object{}, errors.New("it is a #! script, and its interpreter is the program to run")
	}
	f, err := elf.NewFile(r)
	if err != nil {
		return object{}, fmt.Errorf("it is not an ELF file: %w", err)
	}
	o = object{class: f.Class, machine: f.Machine}
	var dynamic []*elf.Prog
	for _, p := range f.Progs {
		switch {
		case p.Type == elf.PT_DYNAMIC:
			dynamic = append(dynamic, p)
		case p.Type == elf.PT_INTERP && o.interp != "":
			return object{}, errors.New("it names more than one loader, PT_INTERP")
		case p.Type == elf.PT_INTERP && p.Filesz <= maxInterp:
			name := make([]byte, p.Filesz)
			if _, err := p.ReadAt(name, 0); err != nil {
				return object{}, fmt.Errorf("the name of its loader cannot be read: %w", err)
			}
			if o.interp, _, _ = strings.Cut(string(name), "\x00"); o.interp != "" {
				continue
			}
			fallthrough
		case p.Type == elf.PT_INTERP:
			return object{}, fmt.Errorf("the name of its loader is empty, or over %d bytes", maxInterp)
		}
	}
	if dynamic != nil {
		o.unread = o.readDynamic(f, dynamic, page)
	}
	return o, nil
}

// readDynamic fills o from f's dynamic segment, dynamic, as the loaders read it: at the segment's
// address, never by a section header, its entries up to the first DT_NULL, and each string at its
// offset in the table DT_STRTAB and DT_STRSZ name, each read from the file as its PT_LOAD
// segments map it. A tag given twice counts as the loaders count it, by its last value. o is a
// library when it is a shared object, ET_DYN, and no program, DF_1_PIE, which glibc refuses to
// load as one. It says why it cannot read them, and then fills nothing: a second dynamic segment,
// PT_LOAD segments out of order or sharing a page, where the bytes at an address are those of the
// segment mapped last, an address no PT_LOAD maps from the file, a segment or table over
// maxDynamic bytes, no DT_NULL within the segment, and a string outside its table.
func (o *object) readDynamic(f *elf.File, dynamic []*elf.Prog, page uint64) error {
	if len(dynamic) > 1 {
		return errors.New("it has more than one dynamic segment, PT_DYNAMIC")
	}
	loads, err := loadSegments(f.Progs, page)
	if err != nil {
		return err
	}
	entries, err := mapped(loads, dynamic[0].Vaddr, dynamic[0].Filesz)
	if err != nil {
		return fmt.Errorf("its dynamic segment %w", err)
	}
	d, needed, err := dynamicEntries(f, entries)
	if err != nil {
		return err
	}
	if at, ok := d.last[elf.DT_STRTAB]; ok {
		if d.table, err = mapped(loads, at, d.last[elf.DT_STRSZ]); err != nil {
			return fmt.Errorf("its string table %w", err)
		}
	}
	read := object{class: o.class, machine: o.machine, interp: o.interp}
	for _, off := range needed {
		name, err := d.str(off)
		if err != nil {
			return err
		}
		read.needed = append(read.needed, name)
	}
	if read.rpath, err = d.list(elf.DT_RPATH); err != nil {
		return err
	}
	if read.runpath, err = d.list(elf.DT_RUNPATH); err != nil {
		return err
	}
	if soname, ok := d.last[elf.DT_SONAME]; ok {
		if read.soname, err = d.str(soname); err != nil {
			return err
		}
	}
	read.library = f.Type == elf.ET_DYN && d.last[elf.DT_FLAGS_1]&uint64(elf.DF_1_PIE) == 0
	*o = read
	return nil
}

// dynamicTable is a dynamic segment's entries, by the last value of each tag, and its string table.
type dynamicTable struct {
	last  map[elf.DynTag]uint64
	table []byte
}

// dynamicEntries reads entries, a dynamic segment, in f's class and byte order up to its first
// DT_NULL: the last value of each tag, and the value of each DT_NEEDED in order.
func dynamicEntries(f *elf.File, entries []byte) (dynamicTable, []uint64, error) {
	size := 16
	if f.Class == elf.ELFCLASS32 {
		size = 8
	}
	word := func(b []byte) uint64 {
		if size == 8 {
			return uint64(f.ByteOrder.Uint32(b))
		}
		return f.ByteOrder.Uint64(b)
	}
	d, needed := dynamicTable{last: map[elf.DynTag]uint64{}}, []uint64(nil)
	for at := 0; at+size <= len(entries); at += size {
		tag, val := elf.DynTag(word(entries[at:])), word(entries[at+size/2:]) //nolint:gosec // G115: a tag past an int's range is one no loader knows, and none this reads
		if tag == elf.DT_NULL {
			return d, needed, nil
		}
		if tag == elf.DT_NEEDED {
			needed = append(needed, val)
		}
		d.last[tag] = val
	}
	return dynamicTable{}, nil, errors.New("its dynamic segment has no DT_NULL within it")
}

// str is the string at the offset off in d's string table, or why it is outside it.
func (d dynamicTable) str(off uint64) (string, error) {
	if off < uint64(len(d.table)) {
		if s, _, ok := bytes.Cut(d.table[off:], []byte{0}); ok {
			return string(s), nil
		}
	}
	return "", fmt.Errorf("its dynamic segment names a string at %d, outside its string table", off)
}

// list is the search list the tag names, split at each colon, and nil when d has no such tag.
func (d dynamicTable) list(tag elf.DynTag) ([]string, error) {
	off, ok := d.last[tag]
	if !ok {
		return nil, nil
	}
	s, err := d.str(off)
	return strings.Split(s, ":"), err
}

// loadSegments is the PT_LOAD segments of progs, which are to be in the order of their addresses
// and share no page of page bytes, so that each byte mapped is one byte of the file whichever of
// them is mapped last, and to hold no more of the file than of memory.
func loadSegments(progs []*elf.Prog, page uint64) ([]*elf.Prog, error) {
	var loads []*elf.Prog
	var end uint64
	for _, p := range progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Filesz > p.Memsz || p.Vaddr > math.MaxUint64-page || p.Memsz > math.MaxUint64-page-p.Vaddr ||
			loads != nil && p.Vaddr&^(page-1) < end {
			return nil, errors.New("its PT_LOAD segments are out of order, share a page, or hold more of the file than of memory")
		}
		end = (p.Vaddr + p.Memsz + page - 1) &^ (page - 1)
		loads = append(loads, p)
	}
	return loads, nil
}

// mapped is the size bytes at the address addr as the PT_LOAD segments loads map the file, all
// read from one segment's bytes of the file, or why they cannot be.
func mapped(loads []*elf.Prog, addr, size uint64) ([]byte, error) {
	if size > maxDynamic {
		return nil, fmt.Errorf("is over %d bytes", maxDynamic)
	}
	for _, p := range loads {
		if addr < p.Vaddr || addr-p.Vaddr > p.Filesz || size > p.Filesz-(addr-p.Vaddr) {
			continue
		}
		b := make([]byte, size)
		if _, err := p.ReadAt(b, int64(addr-p.Vaddr)); err != nil { //nolint:gosec // G115: an offset past an int64 is negative, and a segment reads none there but an error
			return nil, fmt.Errorf("cannot be read: %w", err)
		}
		return b, nil
	}
	return nil, errors.New("is at an address no PT_LOAD segment maps from the file")
}

// loaderNames is, for one architecture, the loader a glibc program names and the one a musl
// program names, the library folder of Debian's multiarch layout and glibc's $PLATFORM.
type loaderNames struct{ glibc, musl, multiarch, platform string }

// loadersOf is the loaders the search knows for the architecture arch, and false for one it holds
// none for, where the block holds no filter either.
func loadersOf(arch string) (loaderNames, bool) {
	switch arch {
	case "amd64":
		return loaderNames{"/lib64/ld-linux-x86-64.so.2", "/lib/ld-musl-x86_64.so.1", "lib/x86_64-linux-gnu", "x86_64"}, true
	case "arm64":
		return loaderNames{"/lib/ld-linux-aarch64.so.1", "/lib/ld-musl-aarch64.so.1", "lib/aarch64-linux-gnu", "aarch64"}, true
	}
	return loaderNames{}, false
}

// loaderRules is one loader's search: whether it is musl's, its built-in folders, which it looks
// in last, and what each $NAME of a list but $ORIGIN stands for.
type loaderRules struct {
	musl     bool
	defaults []string
	tokens   map[string]string
}

// rules is the search of the loader a program names as interp, whose real folder is dir, and false
// for a loader the search does not know. musl's built-in folders are its own list. glibc's, and
// its $LIB, are its distribution's choice, which the loader's real folder tells: Debian's
// multiarch folder, with lib after it, or lib64, or lib, each at the root and under /usr. A glibc
// loader in any other folder is not known.
func (l loaderNames) rules(interp, dir string) (loaderRules, bool) {
	if interp == l.musl {
		return loaderRules{musl: true, defaults: []string{"/lib", "/usr/local/lib", "/usr/lib"}}, true
	}
	for _, lib := range []string{l.multiarch, "lib64", "lib"} {
		if interp != l.glibc || dir != "/"+lib && dir != "/usr/"+lib {
			continue
		}
		defaults := []string{"/" + lib, "/usr/" + lib}
		if lib == l.multiarch {
			defaults = append(defaults, "/lib", "/usr/lib")
		}
		return loaderRules{defaults: defaults, tokens: map[string]string{"LIB": lib, "PLATFORM": l.platform}}, true
	}
	return loaderRules{}, false
}

// muslOwn says whether name is a library musl's loader holds itself, so that no file is looked for.
func muslOwn(name string) bool {
	rest, lib := strings.CutPrefix(name, "lib")
	return lib && !strings.Contains(name, "/") && slices.ContainsFunc(
		[]string{"c.", "pthread.", "rt.", "m.", "dl.", "util.", "xnet."},
		func(own string) bool { return strings.HasPrefix(rest, own) })
}

// tooWide says whether folder, a real folder, is the root or /usr itself: the whole tree, or its
// system half, which no load grants, for a loader's folder as for a library's.
func tooWide(folder string) bool { return folder == "/" || folder == "/usr" }

// noLoadf is ErrUsage for the program prog, whose load folders cannot be told, and why.
func noLoadf(prog, why string, args ...any) error {
	return fmt.Errorf("the program %q cannot be given the folders it loads from: %s: %w", prog, fmt.Sprintf(why, args...), errUsage)
}

// loaded is a file of the search: the path the loader finds it by, whose folder is its $ORIGIN,
// what was read of it, the file that first needed it, nil for the program, and how many files
// lie between it and the program.
type loaded struct {
	object
	name  string
	by    *loaded
	depth int
}

// search is one program's search: the files it reads, the program as the caller named it, its
// loader's rules, the program itself, each name and real path a file is loaded by already, and the
// folders answered so far.
type search struct {
	files loadFiles
	prog  string
	rules loaderRules
	main  *loaded
	known map[string]bool
	dirs  []string
}

// loadFolders is the folders the program prog, an absolute path, needs to load on Linux for the
// machine arch, as files holds them, each by its real path: none for a program that names no
// loader, as a Go program built without cgo, and none for a machine the block holds no filter
// for, which the block refuses itself. A file that is no ELF file, a #! script, a loader the
// search does not know, a list with a $NAME it does not know or a folder that is not an absolute
// path, a library that is not found and a search over its bounds are ErrUsage, naming the file.
func loadFolders(files loadFiles, arch, prog string) ([]string, error) {
	if _, ok := loadersOf(arch); !ok {
		return nil, nil
	}
	resolved, err := files.resolve(prog)
	if err != nil {
		return nil, noLoadf(prog, "it cannot be found: %v", err)
	}
	return loadFoldersOf(files, arch, prog, resolved)
}

// loadFoldersOf is loadFolders for the program prog whose real file is resolved, as RunHolding's
// walk of its path found it, so the folders held and the folders it loads from are told of one
// file, never of two reads of the path.
func loadFoldersOf(files loadFiles, arch, prog, resolved string) ([]string, error) {
	names, ok := loadersOf(arch)
	if !ok {
		return nil, nil
	}
	o, err := files.object(resolved)
	if err != nil {
		return nil, noLoadf(prog, "%v", err)
	}
	if o.interp == "" {
		return nil, nil
	}
	if o.unread != nil {
		return nil, noLoadf(prog, "%v", o.unread)
	}
	loader, err := files.resolve(o.interp)
	if err != nil {
		return nil, noLoadf(prog, "its loader %q cannot be found: %v", o.interp, err)
	}
	if folder := path.Dir(loader); tooWide(folder) {
		return nil, noLoadf(prog, "its loader %q is the file %q, and its folder %q is too wide to grant", o.interp, loader, folder)
	}
	rules, ok := names.rules(o.interp, path.Dir(loader))
	if !ok {
		return nil, noLoadf(prog, "its loader %q, the file %q, is one netblock does not know", o.interp, loader)
	}
	s := &search{files: files, prog: prog, rules: rules, main: &loaded{object: o, name: resolved},
		known: map[string]bool{path.Base(o.interp): true, loader: true}, dirs: []string{path.Dir(loader)}}
	return s.run()
}

// run loads what the program needs, then what each file loaded needs, in the loader's order, and
// answers the folders.
func (s *search) run() ([]string, error) {
	queue := []*loaded{s.main}
	for i := 0; i < len(queue); i++ {
		for _, name := range queue[i].needed {
			if s.known[name] || s.rules.musl && muslOwn(name) {
				continue
			}
			lib, err := s.find(queue[i], name)
			if err != nil {
				return nil, err
			}
			s.known[name] = true
			if lib == nil {
				continue
			}
			if len(queue) > maxLibraries {
				return nil, noLoadf(s.prog, "it loads more than %d libraries", maxLibraries)
			}
			queue = append(queue, lib)
		}
	}
	return s.dirs, nil
}

// find is the file the loader loads for name, which by needs, with its folder answered: nil for a
// file loaded already under another name. A name with a slash is the file's own path.
func (s *search) find(by *loaded, name string) (*loaded, error) {
	if by.depth >= maxLoadDepth {
		return nil, noLoadf(s.prog, "the library %q is more than %d files from the program", name, maxLoadDepth)
	}
	dirs := []string{""}
	switch {
	case !strings.Contains(name, "/"):
		var err error
		if dirs, err = s.searched(by); err != nil {
			return nil, err
		}
	case !path.IsAbs(name):
		return nil, noLoadf(s.prog, "%q needs the library %q, a path that is not absolute", by.name, name)
	}
	for _, dir := range dirs {
		found := path.Join(dir, name)
		resolved, err := s.files.resolve(found)
		if err != nil {
			continue
		}
		if s.known[resolved] {
			return nil, nil
		}
		o, err := s.read(by, found, resolved)
		if err != nil {
			return nil, err
		}
		if o == nil {
			continue
		}
		s.known[resolved] = true
		if !s.rules.musl && o.soname != "" {
			s.known[o.soname] = true
		}
		if folder := path.Dir(resolved); !slices.Contains(s.dirs, folder) {
			s.dirs = append(s.dirs, folder)
		}
		return &loaded{object: *o, name: found, by: by, depth: by.depth + 1}, nil
	}
	return nil, noLoadf(s.prog, "the library %q, which %q needs, is not found in %q", name, by.name, dirs)
}

// read is the file found, at its real path: nil for one of another architecture, which glibc
// passes over and musl refuses. One of the program's architecture is refused when it is no
// library, a program or a file of another type, and when its real folder is the root or /usr
// itself, which no load grants.
func (s *search) read(by *loaded, found, resolved string) (*object, error) {
	o, err := s.files.object(resolved)
	if err = errors.Join(err, o.unread); err != nil {
		return nil, noLoadf(s.prog, "the library %q, which %q needs: %v", found, by.name, err)
	}
	if o.class == s.main.class && o.machine == s.main.machine {
		if !o.library {
			return nil, noLoadf(s.prog, "the library %q, which %q needs, is a program or another file a loader does not load as a library", found, by.name)
		}
		if folder := path.Dir(resolved); tooWide(folder) {
			return nil, noLoadf(s.prog, "the library %q, which %q needs, is the file %q, and its folder %q is too wide to grant", found, by.name, resolved, folder)
		}
		return &o, nil
	}
	if s.rules.musl {
		return nil, noLoadf(s.prog, "the library %q, which %q needs, is of another architecture", found, by.name)
	}
	return nil, nil
}

// searched is the folders the loader looks in for a library by needs, in its order.
func (s *search) searched(by *loaded) ([]string, error) {
	var dirs []string
	add := func(o *loaded, list []string) error {
		expanded, err := s.expand(o, list)
		dirs = append(dirs, expanded...)
		return err
	}
	for o := by; o != nil; o = o.by {
		list := o.rpath
		if s.rules.musl && o.runpath != nil {
			list = o.runpath
		} else if !s.rules.musl && (by.runpath != nil || o.runpath != nil) {
			list = nil
		}
		if err := add(o, list); err != nil {
			return nil, err
		}
	}
	if !s.rules.musl {
		if err := add(by, by.runpath); err != nil {
			return nil, err
		}
	}
	return append(dirs, s.rules.defaults...), nil
}

// expand is the folders of list, a DT_RPATH or DT_RUNPATH of o, with each $NAME or ${NAME} in it
// replaced by what it stands for. A name the loader's rules do not hold, a folder that is not an
// absolute path, the empty one among them, which a loader reads as the working folder, one with a
// line end in it, where musl ends a folder, and a list over the bound are refused.
func (s *search) expand(o *loaded, list []string) ([]string, error) {
	if len(list) > maxPathList {
		return nil, noLoadf(s.prog, "a search list of %q names more than %d folders", o.name, maxPathList)
	}
	dirs := make([]string, 0, len(list))
	for _, entry := range list {
		dir, rest := "", entry
		for {
			before, after, token := strings.Cut(rest, "$")
			dir += before
			if !token {
				break
			}
			name := after[:len(after)-len(strings.TrimLeft(after, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_"))]
			rest = after[len(name):]
			if braced, inner := strings.HasPrefix(after, "{"), strings.Index(after, "}"); braced && inner > 0 {
				name, rest = after[1:inner], after[inner+1:]
			}
			value, ok := s.rules.tokens[name]
			if name == "ORIGIN" {
				value, ok = path.Dir(o.name), true
			}
			if !ok {
				return nil, noLoadf(s.prog, "the search list entry %q of %q holds a $ name netblock does not know", entry, o.name)
			}
			dir += value
		}
		if !path.IsAbs(dir) || strings.Contains(dir, "\n") {
			return nil, noLoadf(s.prog, "the search list entry %q of %q is not one absolute path", entry, o.name)
		}
		dirs = append(dirs, path.Clean(dir))
	}
	return dirs, nil
}
