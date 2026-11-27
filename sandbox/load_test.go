package sandbox

import (
	"bytes"
	"cmp"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The loaders and library folders of the systems the rows fake.
const (
	glibcAMD = "/lib64/ld-linux-x86-64.so.2"
	glibcARM = "/lib/ld-linux-aarch64.so.1"
	muslAMD  = "/lib/ld-musl-x86_64.so.1"
	gnuAMD   = "/usr/lib/x86_64-linux-gnu"
)

// fake is the files of a system that is not there: links, each link, a file's or a folder's, and
// its target, objects, the ELF file at each real path, and broken, the files that are no ELF file
// and why.
type fake struct {
	links   map[string]string
	objects map[string]object
	broken  map[string]string
}

// resolve follows name as the kernel does: one name at a time from the root, a link's target put
// in its place, and ".." taken in the folder reached, so a ".." after a link leaves its target. A
// name of 4096 bytes or more is the kernel's ENAMETOOLONG, and a walk past 40 links its ELOOP.
func (f fake) resolve(name string) (string, error) {
	if len(name) >= 4096 {
		return "", syscall.ENAMETOOLONG
	}
	cur, rest := "/", strings.Split(name, "/")
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			continue
		}
		cur = path.Join(cur, part)
		to, ok := f.links[cur]
		if !ok {
			continue
		}
		if hops++; hops > 40 {
			return "", syscall.ELOOP
		}
		if path.IsAbs(to) {
			cur = "/"
		} else {
			cur = path.Dir(cur)
		}
		rest = append(strings.Split(to, "/"), rest...)
	}
	_, object := f.objects[cur]
	if _, broken := f.broken[cur]; !object && !broken {
		return "", fs.ErrNotExist
	}
	if len(cur) >= 4096 {
		return "", fmt.Errorf("%w: lstat %s: file name too long", errUntold, cur)
	}
	return cur, nil
}

func (f fake) object(name string) (object, error) {
	if why, ok := f.broken[name]; ok {
		return object{}, errors.New(why)
	}
	o, ok := f.objects[name]
	if !ok {
		return object{}, fs.ErrNotExist
	}
	return o, nil
}

// file is f with the ELF file o at the real path name, and link f with the path from resolving to.
func (f fake) file(name string, o object) fake { f.objects[name] = o; return f }
func (f fake) link(from, to string) fake       { f.links[from] = to; return f }

// obj is an amd64 shared library that names the loader interp, "" for none, and needs needed. rp,
// run and so are it with a DT_RPATH, a DT_RUNPATH and a DT_SONAME.
func obj(interp string, needed ...string) object {
	return object{class: elf.ELFCLASS64, machine: elf.EM_X86_64, interp: interp, needed: needed, library: true}
}
func (o object) rp(list ...string) object  { o.rpath = list; return o }
func (o object) run(list ...string) object { o.runpath = list; return o }
func (o object) so(name string) object     { o.soname = name; return o }

// by is o naming a library by the tag the search does not follow.
func (o object) by(tag elf.DynTag) object { o.unfollowed = tag; return o }

// dir is f with a folder at name, which resolve finds as it finds a file.
func (f fake) dir(name string) fake { f.objects[name] = object{}; return f }

// system is a system's loader and libc: the loader named interp, a link to its real folder dir
// unless it is there, and a libc in dir, found through the folder libs when that is another.
func system(interp, dir, libs string) fake {
	f := fake{links: map[string]string{}, objects: map[string]object{}, broken: map[string]string{}}
	loader := dir + interp[strings.LastIndex(interp, "/"):]
	if loader != interp {
		f.link(interp, loader)
	}
	if libs != dir {
		f.link(libs+"/libc.so.6", dir+"/libc.so.6")
	}
	return f.file(loader, obj("")).file(dir+"/libc.so.6", obj("", loader[strings.LastIndex(loader, "/")+1:]).so("libc.so.6"))
}

// debian, fedora, alpine and debianARM are the layouts the rows search: Debian's multiarch folder
// behind /lib64 and /lib, Fedora's /usr/lib64 behind /lib64, and Alpine's musl in /lib.
func debian() fake { return system(glibcAMD, gnuAMD, "/lib/x86_64-linux-gnu") }
func fedora() fake { return system(glibcAMD, "/usr/lib64", "/lib64") }
func alpine() fake { return system(muslAMD, "/lib", "/lib") }
func debianARM() fake {
	return system(glibcARM, "/usr/lib/aarch64-linux-gnu", "/lib/aarch64-linux-gnu")
}

// chain is f with n libraries in dir, lib0 to lib<n-1>, each needing the next.
func chain(f fake, dir string, n int) fake {
	for i := range n {
		o := obj("")
		if i < n-1 {
			o.needed = []string{"lib" + strconv.Itoa(i+1) + ".so"}
		}
		f.file(dir+"/lib"+strconv.Itoa(i)+".so", o)
	}
	return f
}

// many is n names of libraries, lib0.so on, and n folders, /opt/0 on.
func many(n int, format string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(format, i)
	}
	return out
}

// TestTheLoadFoldersAreTheLoadersOwnSearch: the folders a program needs to load are its loader's
// real folder and the real folder of each library it needs, found as that loader finds it with no
// cache. A program that names no loader adds none. glibc's DT_RPATH is handed down to a library
// that has no DT_RUNPATH and passed over in a file that has one, $ORIGIN, $LIB and $PLATFORM stand
// for the folder of the file that names them, the distribution's library folder and the machine, a
// file of another architecture is passed over, and a library loaded already, by its DT_SONAME or
// another name, is not looked for. musl hands both lists down, takes DT_RUNPATH first, and holds
// its own libraries. A library found through a link is held by its real folder: /lib on a system
// whose /lib is /usr/lib is /usr/lib. A search at each bound, and no further, is answered.
//
// What it costs when this fails: RunHolding grants a folder the loader never reads, or leaves out
// one it does, and the program a caller held to its folders does not start, or reads more.
func TestTheLoadFoldersAreTheLoadersOwnSearch(t *testing.T) {
	t.Parallel()
	const tool = "/opt/app/bin/tool"
	rows := []struct {
		claim, arch string
		files       fake
		prog        string
		want        []string
	}{
		{"a program that names no loader", "amd64", debian().file(tool, object{unread: errors.New("no section")}), tool, nil},
		{"a loader behind a link", "amd64", debian().file(tool, obj(glibcAMD, "libc.so.6")), tool, []string{gnuAMD}},
		{
			"a library by $ORIGIN, from a program named through a link", "amd64",
			debian().link("/usr/local/bin/tool", tool).file(tool, obj(glibcAMD, "libapp.so").run("$ORIGIN/../lib")).file("/opt/app/lib/libapp.so", obj("", "libc.so.6")),
			"/usr/local/bin/tool", []string{gnuAMD, "/opt/app/lib"},
		},
		{
			"${LIB} and $PLATFORM where the loader is in lib64", "amd64",
			fedora().file(tool, obj(glibcAMD, "libp.so", "libc.so.6").run("/opt/${LIB}/$PLATFORM")).file("/opt/lib64/x86_64/libp.so", obj("")),
			tool, []string{"/usr/lib64", "/opt/lib64/x86_64"},
		},
		{
			"$LIB where the loader is in the multiarch folder", "amd64",
			debian().file(tool, obj(glibcAMD, "libp.so").run("/opt/$LIB")).file("/opt/lib/x86_64-linux-gnu/libp.so", obj("")),
			tool, []string{gnuAMD, "/opt/lib/x86_64-linux-gnu"},
		},
		{
			"$LIB and $PLATFORM on arm64", "arm64",
			debianARM().file(tool, obj(glibcARM, "libp.so", "libc.so.6").run("/opt/$LIB/$PLATFORM")).file("/opt/lib/aarch64-linux-gnu/aarch64/libp.so", obj("")),
			tool, []string{"/usr/lib/aarch64-linux-gnu", "/opt/lib/aarch64-linux-gnu/aarch64"},
		},
		{
			"the program's DT_RPATH, handed down to a library with no DT_RUNPATH", "amd64",
			debian().file(tool, obj(glibcAMD, "liba.so").rp("/opt/a", "/opt/b")).file("/opt/a/liba.so", obj("", "libb.so")).file("/opt/b/libb.so", obj("")),
			tool, []string{gnuAMD, "/opt/a", "/opt/b"},
		},
		{
			"no DT_RPATH handed down to a library with a DT_RUNPATH", "amd64",
			debian().file(tool, obj(glibcAMD, "liba.so").rp("/opt/a", "/opt/b")).file("/opt/a/liba.so", obj("", "libb.so").run("/opt/c")).
				file("/opt/b/libb.so", obj("")).file("/opt/c/libb.so", obj("")),
			tool, []string{gnuAMD, "/opt/a", "/opt/c"},
		},
		{
			"the DT_RPATH of a file that has a DT_RUNPATH, passed over", "amd64",
			debian().file(tool, obj(glibcAMD, "libx.so").rp("/opt/a").run("/opt/b")).file("/opt/a/libx.so", obj("")).file("/opt/b/libx.so", obj("")),
			tool, []string{gnuAMD, "/opt/b"},
		},
		{
			"the DT_RPATH of a program that has a DT_RUNPATH, not handed down either", "amd64",
			debian().file(tool, obj(glibcAMD, "liba.so").rp("/opt/a").run("/opt/r")).file("/opt/r/liba.so", obj("", "libb.so")).
				file("/opt/a/libb.so", obj("")).file(gnuAMD+"/libb.so", obj("")),
			tool, []string{gnuAMD, "/opt/r"},
		},
		{
			"a file of another architecture, passed over", "amd64",
			debian().file(tool, obj(glibcAMD, "libx.so").rp("/opt/a32", "/opt/a")).file("/opt/a32/libx.so", object{class: elf.ELFCLASS32, machine: elf.EM_386}).file("/opt/a/libx.so", obj("")),
			tool, []string{gnuAMD, "/opt/a"},
		},
		{
			"a library loaded already by its DT_SONAME, and one by another name", "amd64",
			debian().file(tool, obj(glibcAMD, "libfoo.so", "libbar.so.1", "libz.so", "libz.so.1")).file(gnuAMD+"/libfoo.so", obj("").so("libbar.so.1")).
				link("/lib/x86_64-linux-gnu/libfoo.so", gnuAMD+"/libfoo.so").link("/lib/x86_64-linux-gnu/libz.so", gnuAMD+"/libz.so.1.3").
				link("/lib/x86_64-linux-gnu/libz.so.1", gnuAMD+"/libz.so.1.3").file(gnuAMD+"/libz.so.1.3", obj("")),
			tool, []string{gnuAMD},
		},
		{
			"a library found through /lib where it is /usr/lib, by its real folder", "amd64",
			debian().file(tool, obj(glibcAMD, "libold.so")).link("/lib/libold.so", "/usr/lib/libold.so").file("/usr/lib/libold.so", obj("")),
			tool, []string{gnuAMD, "/usr/lib"},
		},
		{
			"a library in glibc's last built-in folder, and one named by its path", "amd64",
			debian().file(tool, obj(glibcAMD, "libold.so", "/opt/x/libp.so")).file("/usr/lib/libold.so", obj("")).file("/opt/x/libp.so", obj("")),
			tool, []string{gnuAMD, "/usr/lib", "/opt/x"},
		},
		{
			"musl: its own libraries are the loader, and its built-in folders", "amd64",
			alpine().file(tool, obj(muslAMD, "libz.so.1", "libc.musl-x86_64.so.1", "libm.so.6")).file("/usr/lib/libz.so.1", obj("", "libc.musl-x86_64.so.1")),
			tool, []string{"/lib", "/usr/lib"},
		},
		{
			"musl: DT_RUNPATH before DT_RPATH in one file, and handed down", "amd64",
			alpine().file(tool, obj(muslAMD, "liba.so").rp("/opt/no").run("${ORIGIN}/../a")).file("/opt/no/liba.so", obj("")).
				file("/opt/app/a/liba.so", obj("", "libb.so")).file("/opt/app/a/libb.so", obj("")),
			tool, []string{"/lib", "/opt/app/a"},
		},
		{"a machine the block holds no filter for, whose program is not read", "riscv64", fake{}, tool, nil},
		{
			"a library maxLoadDepth files from the program", "amd64",
			chain(debian().file(tool, obj(glibcAMD, "lib0.so").rp("/opt/c")), "/opt/c", maxLoadDepth), tool, []string{gnuAMD, "/opt/c"},
		},
		{
			"maxLibraries libraries, by a list of maxPathList folders", "amd64",
			chain(debian().file(tool, obj(glibcAMD, many(maxLibraries, "lib%d.so")...).rp(many(maxPathList, "/opt/%d")...)), "/opt/63", maxLibraries),
			tool, []string{gnuAMD, "/opt/63"},
		},
	}
	for _, r := range rows {
		got, err := loadFolders(r.files, r.arch, r.prog, []string{"/opt"})
		if err != nil || !slices.Equal(got, r.want) {
			t.Errorf("%s: the folders %q, %v, want %q", r.claim, got, err, r.want)
		}
	}
	if len(rows) < 19 {
		t.Fatalf("ran %d rows", len(rows))
	}
}

// TestAProgramWhoseLoadFoldersCannotBeToldIsRefused: a program that is not there, a file that is no
// ELF file, a #! script, a dynamic segment that cannot be read, a loader that is not there, whose
// real folder is the root or /usr itself, directly or through a link, or that is neither glibc's
// nor musl's for the machine, a glibc loader in a folder no layout has, a library that is not
// found, that is no ELF file, that is a program or no library, whose real folder is the root or
// /usr itself, directly or through a link, that is named by a path from the working folder,
// a DT_RUNPATH glibc does not hand down, a search list with a $ name the loader does not know, with a
// folder that is empty, not absolute or holds a line end, and a search one past each bound are
// each ErrUsage, naming the program and the file or the name, with no folder answered. None
// matches ErrNoBlock.
//
// What it costs when this fails: a program the search cannot follow runs behind a grant that was
// guessed, or the caller is told the block is missing when its program is what was refused.
func TestAProgramWhoseLoadFoldersCannotBeToldIsRefused(t *testing.T) {
	t.Parallel()
	const tool = "/opt/app/bin/tool"
	glibc := func(o object, more ...string) fake {
		f := debian().file(tool, o)
		for _, name := range more {
			f.file(name, obj(""))
		}
		return f
	}
	rows := []struct {
		claim string
		files fake
		says  string
	}{
		{"a program that is not there", debian(), "it cannot be found"},
		{"a file that is no ELF file", fake{broken: map[string]string{tool: "it is not an ELF file"}}, "it is not an ELF file"},
		{"a #! script", fake{broken: map[string]string{tool: "it is a #! script"}}, "it is a #! script"},
		{"a dynamic segment that cannot be read", glibc(object{interp: glibcAMD, unread: errors.New("no dynamic segment")}), "no dynamic segment"},
		{"a loader that is not there", fake{objects: map[string]object{tool: obj(glibcAMD)}}, `its loader "` + glibcAMD + `" cannot be found`},
		{"a loader that is neither glibc's nor musl's", glibc(obj("/lib/ld.so.1"), "/lib/ld.so.1"), `"/lib/ld.so.1", is one netblock does not know`},
		{"the loader of another machine", debianARM().file(tool, obj(glibcARM)), "is one netblock does not know"},
		{"a glibc loader in a folder no layout has", system(glibcAMD, "/opt/glibc/lib", "/opt/glibc/lib").file(tool, obj(glibcAMD)), `"/opt/glibc/lib/ld-linux-x86-64.so.2", is one netblock does not know`},
		{"a library that is not found", glibc(obj(glibcAMD, "libc.so.6", "libgone.so").rp("/opt/a")), `the library "libgone.so", which "` + tool + `" needs, is not found in ["/opt/a" "/lib/x86_64-linux-gnu"`},
		{"a library that is no ELF file", func() fake {
			f := glibc(obj(glibcAMD, "libbad.so").rp("/opt/a"))
			f.broken["/opt/a/libbad.so"] = "it is not an ELF file"
			return f
		}(), `the library "/opt/a/libbad.so"`},
		{"a library whose dynamic segment cannot be read", glibc(obj(glibcAMD, "libx.so").rp("/opt/a")).file("/opt/a/libx.so", object{unread: errors.New("no dynamic segment")}), `the library "/opt/a/libx.so"`},
		{"a library named by a path from the working folder", glibc(obj(glibcAMD, "sub/libx.so")), `the library "sub/libx.so", a path that is not absolute`},
		{"a DT_RUNPATH, which glibc does not hand down", glibc(obj(glibcAMD, "liba.so").run("/opt/a"), "/opt/a/libb.so").file("/opt/a/liba.so", obj("", "libb.so")), `the library "libb.so", which "/opt/a/liba.so" needs, is not found`},
		{"a $ name glibc does not know", glibc(obj(glibcAMD, "libx.so").run("$HOME/lib")), `"$HOME/lib" of "` + tool + `" holds a $ name`},
		{"a ${ with no end", glibc(obj(glibcAMD, "libx.so").run("${ORIGIN/lib")), "holds a $ name"},
		{"$LIB, which musl does not know", alpine().file(tool, obj(muslAMD, "libx.so").rp("/opt/$LIB")), `"/opt/$LIB" of "` + tool + `" holds a $ name`},
		{"an empty folder in a list", glibc(obj(glibcAMD, "libx.so").rp("/opt/a", ""), "/opt/a/libx.so"), `the search list entry "" of`},
		{"a folder from the working folder", glibc(obj(glibcAMD, "libx.so").run("lib")), `the search list entry "lib" of`},
		{"a folder with a line end in it", alpine().file(tool, obj(muslAMD, "libx.so").run("/opt/a\n/opt/b")), `the search list entry "/opt/a\n/opt/b" of`},
		{"musl: a library of another architecture", alpine().file(tool, obj(muslAMD, "libx.so")).file("/lib/libx.so", object{class: elf.ELFCLASS32}), `the library "/lib/libx.so", which "` + tool + `" needs, is of another architecture`},
		{"a file found that is no library", glibc(obj(glibcAMD, "libx.so").rp("/opt/a")).file("/opt/a/libx.so", object{class: elf.ELFCLASS64, machine: elf.EM_X86_64}), `the library "/opt/a/libx.so", which "` + tool + `" needs, is a program or another file a loader does not load as a library`},
		{"a library in the root folder", glibc(obj(glibcAMD, "libx.so").run("/"), "/libx.so"), `the file "/libx.so", and its folder "/" is too wide to grant`},
		{"a library in /usr itself, through a link", glibc(obj(glibcAMD, "libx.so"), "/usr/libx.so").link("/lib/x86_64-linux-gnu/libx.so", "/usr/libx.so"), `the file "/usr/libx.so", and its folder "/usr" is too wide to grant`},
		{"a musl loader in /usr itself, through a link", system(muslAMD, "/usr", "/usr").file(tool, obj(muslAMD)), `its loader "` + muslAMD + `" is the file "/usr/ld-musl-x86_64.so.1", and its folder "/usr" is too wide to grant`},
		{"a musl loader in the root folder, through a link", fake{links: map[string]string{muslAMD: "/ld-musl-x86_64.so.1"}, objects: map[string]object{"/ld-musl-x86_64.so.1": obj(""), tool: obj(muslAMD)}}, `its loader "` + muslAMD + `" is the file "/ld-musl-x86_64.so.1", and its folder "/" is too wide to grant`},
		{"a list one folder past maxPathList", glibc(obj(glibcAMD, "libx.so").rp(many(maxPathList+1, "/opt/%d")...), "/opt/0/libx.so"), "names more than " + strconv.Itoa(maxPathList) + " folders"},
		{"a library one file past maxLoadDepth", chain(glibc(obj(glibcAMD, "lib0.so").rp("/opt/c")), "/opt/c", maxLoadDepth+1), `the library "lib` + strconv.Itoa(maxLoadDepth) + `.so" is more than ` + strconv.Itoa(maxLoadDepth) + " files from the program"},
		{"one library past maxLibraries", chain(glibc(obj(glibcAMD, many(maxLibraries+1, "lib%d.so")...).rp("/opt/c")), "/opt/c", maxLibraries+1), "it loads more than " + strconv.Itoa(maxLibraries) + " libraries"},
	}
	for _, r := range rows {
		arch := "amd64"
		got, err := loadFolders(r.files, arch, tool, []string{"/opt"})
		if got != nil {
			t.Errorf("%s: the folders %q with the refusal, want none", r.claim, got)
		}
		if !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) {
			t.Errorf("%s: %v, want ErrUsage and never ErrNoBlock", r.claim, err)
		}
		if says := fmt.Sprint(err); !strings.Contains(says, `the program "`+tool+`"`) || !strings.Contains(says, r.says) {
			t.Errorf("%s: %v, want the program named and %q", r.claim, err, r.says)
		}
	}
	if len(rows) < 28 {
		t.Fatalf("ran %d rows", len(rows))
	}
}

// TestALibraryBeneathNoFolderHeldAndNoBuiltInFolderOfTheLoaderIsRefused: with the caller holding
// /opt/app, the program's own folder among it, a library whose real folder is beneath neither that
// folder nor a built-in folder of the loader is ErrUsage, naming the program, the library, the file
// that needs it, its real file and its folder, with no folder answered: one the program names by
// its full path; one found by the program's run path; one found by a library's run path, in a
// folder held, that names it by its full path, and one found by that library's own run path; one
// found through a link in a folder held, by its real file; one in a folder whose name only starts
// with the folder held; one in /usr/local/lib, a built-in folder of musl's but not of glibc's; one
// found by its plain name in a built-in folder through a link leading out of it, by its real file; and
// musl's, found in the next folder of a run path whose first, in the folder held, makes a path of
// muslPath bytes with the name, which musl never opens. So is one found by a run path whose first
// folder the kernel walks by short links to a real path past its length limit, which the loader
// opens though its real folder cannot be told, ahead of a library of that name in the folder held.
// So is musl's library found by a run path written with its closing slash, whose own run path by
// $ORIGIN, the folder as musl writes it, makes a path of muslPath bytes, so musl passes it over for
// the next folder's; a library looked for as a path the kernel answers with an error on which a
// loader stops, a link loop or a path past its length limit, ahead of one in the folder held; a
// program or a library that names one by DT_AUDIT, DT_DEPAUDIT, DT_FILTER or DT_AUXILIARY, on glibc
// or musl; and, on glibc, a library looked for in a folder whose glibc-hwcaps subfolder is beneath
// no built-in folder by its real path, in the folder held or through a link out of a built-in folder,
// or is a link loop, or is reached by short links at a real path past the length limit, which cannot
// be told.
// A program whose libraries are all beneath the folder held or a built-in folder of its loader
// runs: by $ORIGIN, by a run path into a second folder held, by a run path beneath a built-in
// folder, musl's /usr/local/lib, and with a file of another architecture outside them, which glibc
// passes over, so it is never loaded; musl's from that first folder when its path is a byte
// shorter, and from the folder by $ORIGIN when the run path that found its library has no closing
// slash; on glibc, by a built-in folder with its own glibc-hwcaps subfolder; and on musl, which reads
// no glibc-hwcaps subfolder, by a folder held that has one.
//
// What it costs when this fails: a program written into a read-write folder names a library in a
// folder the caller never named, and RunHolding grants the program the read and the run of all of
// that folder.
func TestALibraryBeneathNoFolderHeldAndNoBuiltInFolderOfTheLoaderIsRefused(t *testing.T) {
	t.Parallel()
	const tool = "/opt/app/bin/tool"
	held := []string{"/opt/app"}
	glibc := func(o object) fake { return debian().file(tool, o) }
	inHeld := "/opt/app/lib/liba.so"
	// muslIn is a folder beneath base, by names of 100 bytes or fewer, whose path with a slash and
	// libx.so is short bytes fewer than muslPath; muslDir is one beneath /opt/app.
	muslIn := func(base string, short int) string {
		dir := base
		for n := muslPath - short - len("/libx.so"); len(dir) < n; {
			dir += "/" + strings.Repeat("d", min(100, n-len(dir)-1))
		}
		return dir
	}
	muslDir := func(short int) string { return muslIn("/opt/app", short) }
	// beyondMusl is musl's program finding libx.so by a run path whose first folder is muslDir(short),
	// in the folder held, and whose second is /elsewhere, each with a libx.so.
	beyondMusl := func(short int) fake {
		return alpine().file(tool, obj(muslAMD, "libx.so").run(muslDir(short), "/elsewhere")).file(muslDir(short)+"/libx.so", obj("")).file("/elsewhere/libx.so", obj(""))
	}
	// padded is a folder beneath /opt/app/lib whose path, as written from that folder, with a slash
	// and libx.so is a byte under muslPath; slashed is musl's program finding libb.so in /opt/app/lib
	// by the run path runpath, and libb.so finding libx.so by $ORIGIN and the rest of padded, then in
	// /elsewhere, each with a libx.so.
	padded := muslIn("/opt/app/lib", 1)
	slashed := func(runpath string) fake {
		return alpine().file(tool, obj(muslAMD, "libb.so").run(runpath)).file("/opt/app/lib/libb.so", obj("", "libx.so").run("$ORIGIN"+padded[len("/opt/app/lib"):], "/elsewhere")).
			file(padded+"/libx.so", obj("")).file("/elsewhere/libx.so", obj(""))
	}
	// stops is glibc's program finding libb.so in /opt/app/lib, then /elsewhere, by DT_RPATH, and
	// libb.so finding libx.so by DT_RPATH in first, then in /opt/app/good, each with a libx.so but
	// first, where loop/libx.so is a link to itself.
	stops := func(first string) fake {
		return glibc(obj(glibcAMD, "libb.so").rp("/opt/app/lib", "/elsewhere")).file("/opt/app/lib/libb.so", obj("", "libx.so").rp(first, "/opt/app/good")).
			link("/opt/app/loop/libx.so", "/opt/app/loop/libx.so").file("/opt/app/good/libx.so", obj("")).file("/elsewhere/libx.so", obj(""))
	}
	long := "/opt/app" + strings.Repeat("/x/..", 820)
	// deep is 25 folders, each a slash and 100 bytes of the letter c: 2,525 bytes, so two of them
	// make a path past the kernel's length limit.
	deep := func(c string) string { return strings.Repeat("/"+strings.Repeat(c, 100), 25) }
	// found is the path the loader finds a library in lib by, through the program's $ORIGIN/../lib,
	// as it writes it.
	found := func(lib string) string { return "/opt/app/bin/../lib/" + lib }
	// behind is the program o, with a link /opt/app/l to /elsewhere/deep, a library in /elsewhere and
	// one of the same name in /opt/app.
	behind := func(o object) fake {
		return glibc(o).link("/opt/app/l", "/elsewhere/deep").file("/elsewhere/liba.so", obj("")).file("/opt/app/liba.so", obj(""))
	}
	refused := []struct {
		claim string
		files fake
		says  string
	}{
		{
			"a library the program names by its full path", glibc(obj(glibcAMD, "/elsewhere/libx.so")).file("/elsewhere/libx.so", obj("")),
			`the library "/elsewhere/libx.so", which "` + tool + `" needs, is the file "/elsewhere/libx.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			"a library the program finds by its run path", glibc(obj(glibcAMD, "libx.so").run("/elsewhere")).file("/elsewhere/libx.so", obj("")),
			`the library "/elsewhere/libx.so", which "` + tool + `" needs, is the file "/elsewhere/libx.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			"a library in a folder held, found by run path, that names one by its full path",
			glibc(obj(glibcAMD, "liba.so").run("$ORIGIN/../lib")).file(inHeld, obj("", "/elsewhere/liby.so")).file("/elsewhere/liby.so", obj("")),
			`the library "/elsewhere/liby.so", which "` + found("liba.so") + `" needs, is the file "/elsewhere/liby.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			"a library in a folder held whose own run path finds one outside",
			glibc(obj(glibcAMD, "liba.so").run("$ORIGIN/../lib")).file(inHeld, obj("", "liby.so").run("/elsewhere")).file("/elsewhere/liby.so", obj("")),
			`the library "/elsewhere/liby.so", which "` + found("liba.so") + `" needs, is the file "/elsewhere/liby.so"`,
		},
		{
			"a link in a folder held on a run path, to a library outside, by its real file",
			glibc(obj(glibcAMD, "liby.so").run("$ORIGIN/../lib")).link("/opt/app/lib/liby.so", "/elsewhere/liby.so.1").file("/elsewhere/liby.so.1", obj("")),
			`the library "` + found("liby.so") + `", which "` + tool + `" needs, is the file "/elsewhere/liby.so.1", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			// A soname is a name, never a real path: one that spells the link's real file does not
			// pass that file over unjudged.
			"the same link, after a library whose soname is the real file it leads to",
			glibc(obj(glibcAMD, "liba.so", "liby.so").run("$ORIGIN/../lib")).file(inHeld, obj("").so("/elsewhere/liby.so.1")).
				link("/opt/app/lib/liby.so", "/elsewhere/liby.so.1").file("/elsewhere/liby.so.1", obj("")),
			`the library "` + found("liby.so") + `", which "` + tool + `" needs, is the file "/elsewhere/liby.so.1", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			"a folder whose name only starts with the folder held", glibc(obj(glibcAMD, "/opt/application/libx.so")).file("/opt/application/libx.so", obj("")),
			`its folder "/opt/application" is beneath no folder held`,
		},
		{
			"glibc: /usr/local/lib, which is musl's built-in folder and not glibc's", glibc(obj(glibcAMD, "libx.so").run("/usr/local/lib")).file("/usr/local/lib/libx.so", obj("")),
			`its folder "/usr/local/lib" is beneath no folder held and no built-in folder of the loader`,
		},
		{
			// A plain name with no run path, found in a built-in folder through a link that leads out
			// of it: judged by the link's real file, as by a full path or a run path.
			"a library found by its plain name in a built-in folder through a link leading out of it",
			glibc(obj(glibcAMD, "libx.so")).link(gnuAMD+"/libx.so", "/elsewhere/libx.so").file("/elsewhere/libx.so", obj("")),
			`the library "` + gnuAMD + `/libx.so", which "` + tool + `" needs, is the file "/elsewhere/libx.so", and its folder "/elsewhere" is beneath no folder held and no built-in folder of the loader`,
		},
		{
			// The loader opens the path as written, and the kernel takes ".." from the link's target,
			// /elsewhere, never from the link's name: the file in /opt/app is a decoy no loader opens.
			"a library named by its full path through a link and \"..\"", behind(obj(glibcAMD, "/opt/app/l/../liba.so")),
			`the library "/opt/app/l/../liba.so", which "` + tool + `" needs, is the file "/elsewhere/liba.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			"a library found by a run path through a link and \"..\"", behind(obj(glibcAMD, "liba.so").run("/opt/app/l/..")),
			`the library "/opt/app/l/../liba.so", which "` + tool + `" needs, is the file "/elsewhere/liba.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			"a library found by an $ORIGIN run path through a link and \"..\"", behind(obj(glibcAMD, "liba.so").run("$ORIGIN/../l/..")),
			`the library "/opt/app/bin/../l/../liba.so", which "` + tool + `" needs, is the file "/elsewhere/liba.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			// musl never opens a path that fills its buffer, and takes the file in the next folder.
			"musl: a run path folder in the folder held whose path with the name is muslPath bytes", beyondMusl(0),
			`the library "/elsewhere/libx.so", which "` + tool + `" needs, is the file "/elsewhere/libx.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			// The kernel walks link by link, so the loader opens the file at /opt/app/rp/bb, whose real
			// path, past the kernel's length limit, cannot be told: the decoy in /opt/app/lib, the next
			// folder, is never the file it loads.
			"a library found by a run path through short links to a real path past the length limit",
			glibc(obj(glibcAMD, "liba.so").run("/opt/app/rp/bb", "/opt/app/lib")).link("/opt/app/rp", "/far"+deep("a")).
				link("/far"+deep("a")+"/bb", deep("b")[1:]).file("/far"+deep("a")+deep("b")+"/liba.so", obj("")).file(inHeld, obj("")),
			`the library "liba.so", which "` + tool + `" needs, is the file "/opt/app/rp/bb/liba.so", and its real path cannot be told`,
		},
		{
			// musl writes the folder as written, a slash and the name, so libb.so's $ORIGIN is
			// /opt/app/lib/, and the folder by it a byte longer than padded: musl passes it over.
			"musl: a library found by a run path with a closing slash, whose $ORIGIN folder is then muslPath bytes with the name", slashed("/opt/app/lib/"),
			`the library "/elsewhere/libx.so", which "/opt/app/lib//libb.so" needs, is the file "/elsewhere/libx.so", and its folder "/elsewhere" is beneath no folder held`,
		},
		{
			// glibc ends libb.so's list at the loop and goes on to the program's DT_RPATH, /elsewhere.
			"glibc: a library looked for as a link loop, ahead of one in the folder held", stops("/opt/app/loop"),
			`the library "libx.so", which "/opt/app/lib/libb.so" needs, is looked for as "/opt/app/loop/libx.so", which the kernel answers with an error on which a loader stops looking`,
		},
		{
			"a library looked for as a path past the kernel's length limit, ahead of one in the folder held", stops(long),
			`is looked for as "` + long + `/libx.so", which the kernel answers with an error on which a loader stops looking: file name too long`,
		},
		{"a program that names a library by DT_AUDIT", glibc(obj(glibcAMD, "libc.so.6").by(elf.DT_AUDIT)), "it names a library by DT_AUDIT, which the search does not follow"},
		{"a program that names a library by DT_DEPAUDIT", glibc(obj(glibcAMD, "libc.so.6").by(elf.DT_DEPAUDIT)), "it names a library by DT_DEPAUDIT, which the search does not follow"},
		{
			"a library in the folder held that names one by DT_FILTER", glibc(obj(glibcAMD, "liba.so").run("$ORIGIN/../lib")).file(inHeld, obj("").by(elf.DT_FILTER)),
			`the library "` + found("liba.so") + `", which "` + tool + `" needs, names a library by DT_FILTER, which the search does not follow`,
		},
		{
			"musl: a library in the folder held that names one by DT_AUXILIARY", alpine().file(tool, obj(muslAMD, "liba.so").run("$ORIGIN/../lib")).file(inHeld, obj("").by(elf.DT_AUXILIARY)),
			`the library "` + found("liba.so") + `", which "` + tool + `" needs, names a library by DT_AUXILIARY, which the search does not follow`,
		},
		{
			"glibc: a library looked for in a folder held with a glibc-hwcaps subfolder", glibc(obj(glibcAMD, "liba.so").run("/opt/app/lib")).dir("/opt/app/lib/glibc-hwcaps").file(inHeld, obj("")),
			`the library "liba.so", which "` + tool + `" needs, is looked for in "/opt/app/lib", where glibc first looks in "/opt/app/lib/glibc-hwcaps" by the machine's levels, which the search does not follow, and its real path "/opt/app/lib/glibc-hwcaps" is beneath no built-in folder of the loader`,
		},
		{
			"glibc: a built-in folder whose glibc-hwcaps subfolder is a link out of it", glibc(obj(glibcAMD, "libc.so.6")).link("/lib/x86_64-linux-gnu/glibc-hwcaps", "/opt/app/hw").dir("/opt/app/hw"),
			`where glibc first looks in "/lib/x86_64-linux-gnu/glibc-hwcaps" by the machine's levels, which the search does not follow, and its real path "/opt/app/hw" is beneath no built-in folder of the loader`,
		},
		{
			"glibc: a folder held whose glibc-hwcaps subfolder is a link loop",
			glibc(obj(glibcAMD, "liba.so").run("/opt/app/lib")).link("/opt/app/lib/glibc-hwcaps", "/opt/app/lib/glibc-hwcaps").file(inHeld, obj("")),
			`where glibc first looks in "/opt/app/lib/glibc-hwcaps" by the machine's levels, which the search does not follow, and too many levels of symbolic links`,
		},
		{
			"glibc: a folder held whose glibc-hwcaps subfolder is reached by short links at a real path past the length limit",
			glibc(obj(glibcAMD, "liba.so").run("/opt/app/lib")).link("/opt/app/lib/glibc-hwcaps", "/far"+deep("a")+"/bb").
				link("/far"+deep("a")+"/bb", deep("b")[1:]).dir("/far"+deep("a")+deep("b")).file(inHeld, obj("")),
			`where glibc first looks in "/opt/app/lib/glibc-hwcaps" by the machine's levels, which the search does not follow, and ` + errUntold.Error(),
		},
	}
	for _, r := range refused {
		got, err := loadFolders(r.files, "amd64", tool, held)
		if got != nil || !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) {
			t.Errorf("%s: the folders %q, %v; want none, ErrUsage and never ErrNoBlock", r.claim, got, err)
		}
		if says := fmt.Sprint(err); !strings.Contains(says, `the program "`+tool+`"`) || !strings.Contains(says, r.says) {
			t.Errorf("%s: %v, want the program named and %q", r.claim, err, r.says)
		}
	}
	runs := []struct {
		claim string
		files fake
		held  []string
		want  []string
	}{
		{"libraries all in the loader's built-in folders", glibc(obj(glibcAMD, "libc.so.6")), held, []string{gnuAMD}},
		{"a library beside the program by $ORIGIN", glibc(obj(glibcAMD, "liba.so").run("$ORIGIN/../lib")).file(inHeld, obj("")), held, []string{gnuAMD, "/opt/app/lib"}},
		{"a run path into a second folder held", glibc(obj(glibcAMD, "libx.so").run("/srv/libs")).file("/srv/libs/libx.so", obj("")), append([]string{"/srv"}, held...), []string{gnuAMD, "/srv/libs"}},
		{"a run path beneath a built-in folder", glibc(obj(glibcAMD, "libp.so").run(gnuAMD+"/pkg")).file(gnuAMD+"/pkg/libp.so", obj("")), held, []string{gnuAMD, gnuAMD + "/pkg"}},
		{"musl: /usr/local/lib, its built-in folder", alpine().file(tool, obj(muslAMD, "libx.so")).file("/usr/local/lib/libx.so", obj("")), held, []string{"/lib", "/usr/local/lib"}},
		{
			"a file of another architecture outside them, which glibc passes over",
			glibc(obj(glibcAMD, "libx.so").rp("/elsewhere", "/opt/app/lib")).file("/elsewhere/libx.so", object{class: elf.ELFCLASS32, machine: elf.EM_386}).file("/opt/app/lib/libx.so", obj("")),
			held, []string{gnuAMD, "/opt/app/lib"},
		},
		{
			// The loader finds libb.so in the folder above liba.so's real folder, /srv/deep, never
			// above the link's name, where there is none.
			"a library through a linked folder whose run path is $ORIGIN/../x",
			glibc(obj(glibcAMD, "liba.so").run("/opt/app/l")).link("/opt/app/l", "/srv/deep").
				file("/srv/deep/liba.so", obj("", "libb.so").run("$ORIGIN/../x")).file("/srv/x/libb.so", obj("")),
			[]string{"/opt/app", "/srv"}, []string{gnuAMD, "/srv/deep", "/srv/x"},
		},
		{
			// liba.so's $ORIGIN is the text of the path it was found by, /opt/app/l/../lib, which
			// the kernel reads as /srv/lib: libb.so is there, and none is in /opt/app/lib.
			"a library found through a link and \"..\" whose run path is $ORIGIN",
			glibc(obj(glibcAMD, "liba.so").run("/opt/app/l/../lib")).link("/opt/app/l", "/srv/deep").
				file("/srv/lib/liba.so", obj("", "libb.so").run("$ORIGIN")).file("/srv/lib/libb.so", obj("")),
			[]string{"/opt/app", "/srv"}, []string{gnuAMD, "/srv/lib"},
		},
		{"musl: a run path folder whose path with the name is a byte under muslPath", beyondMusl(1), held, []string{"/lib", muslDir(1)}},
		{"musl: the same $ORIGIN folder, by a run path with no closing slash", slashed("/opt/app/lib"), held, []string{"/lib", "/opt/app/lib", padded}},
		{"glibc: a built-in folder with its own glibc-hwcaps subfolder", glibc(obj(glibcAMD, "libc.so.6")).dir("/lib/x86_64-linux-gnu/glibc-hwcaps"), held, []string{gnuAMD}},
		{
			"musl: a folder held with a glibc-hwcaps subfolder, which musl does not read",
			alpine().file(tool, obj(muslAMD, "liba.so").run("/opt/app/lib")).dir("/opt/app/lib/glibc-hwcaps").file(inHeld, obj("")), held, []string{"/lib", "/opt/app/lib"},
		},
	}
	for _, r := range runs {
		if got, err := loadFolders(r.files, "amd64", tool, r.held); err != nil || !slices.Equal(got, r.want) {
			t.Errorf("%s: the folders %q, %v, want %q", r.claim, got, err, r.want)
		}
	}
	if len(refused) < 24 || len(runs) < 12 {
		t.Fatalf("ran %d refused rows and %d that run", len(refused), len(runs))
	}
}

// TestTheLoadFoldersAreHeldReadOnlyOnceAndNeverOverAFolderOfTheCallers: a load folder is added to
// the read-only folders, once; one that is a folder the caller named, or beneath one, read-write
// or read-only, is not added, so the caller's grant of it stands as it was; and the caller's own
// lists are not written.
//
// What it costs when this fails: a folder the caller holds read-write is named again read-only, a
// rule the block refuses, or a caller's own list changes under it.
func TestTheLoadFoldersAreHeldReadOnlyOnceAndNeverOverAFolderOfTheCallers(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	p := func(parts ...string) string { return sep + strings.Join(parts, sep) }
	ro := []string{p("usr", "bin"), ""}
	n := named{readWrite: []string{p("work")}, readOnly: ro[:1]}
	got := n.with([]string{p("usr", "lib"), p("work", "lib"), p("usr", "bin"), p("work"), p("usr", "lib"), p("usr", "lib", "sub"), p("usr", "binary")})
	if want := []string{p("usr", "bin"), p("usr", "lib"), p("usr", "binary")}; !slices.Equal(got.readOnly, want) || !slices.Equal(got.readWrite, n.readWrite) {
		t.Errorf("held read-only %q and read-write %q, want %q and %q", got.readOnly, got.readWrite, want, n.readWrite)
	}
	if ro[1] != "" {
		t.Errorf("the caller's own list was written past its end: %q", ro)
	}
}

// dynEntry is one entry of a dynamic segment a row builds: a tag and its string.
type dynEntry struct {
	tag elf.DynTag
	val string
}

// elfSpec is a 64-bit little-endian ELF file a row builds, whose every byte one PT_LOAD segment
// maps at the address of its offset: interp, the bytes of its PT_INTERP, nil for none; dynamic,
// its PT_DYNAMIC's entries, nil for none, after which come a DT_FLAGS_1 of flags1 when it is not
// 0, DT_STRTAB, DT_STRSZ and DT_NULL; decoy, the entries of another dynamic section, which the
// section headers name in the segment's place; noSection, no section header of the dynamic type;
// twoInterps and twoDynamics, each header given twice; exec, ET_EXEC for ET_DYN; noLoad, no
// PT_LOAD; shared, a second PT_LOAD on the first one's page; noEnd, no DT_NULL; and dynSize and
// strSize, the sizes its PT_DYNAMIC and its DT_STRSZ claim, 0 for their own.
type elfSpec struct {
	interp                                   []byte
	dynamic, decoy                           []dynEntry
	flags1, dynSize, strSize                 uint64
	noSection, twoInterps, twoDynamics, exec bool
	noLoad, shared, noEnd                    bool
}

// bytes is the file s describes.
func (s elfSpec) bytes() []byte {
	le := binary.LittleEndian
	twice := func(on, again bool) int {
		switch {
		case !on:
			return 0
		case again:
			return 2
		}
		return 1
	}
	nprogs := twice(s.interp != nil, s.twoInterps) + twice(s.dynamic != nil, s.twoDynamics) + twice(!s.noLoad, s.shared)
	interpAt := uint64(64 + 56*nprogs)
	strAt, strtab := interpAt+uint64(len(s.interp)), []byte{0}
	offsets := func(list []dynEntry) []uint64 {
		var out []uint64
		for _, d := range list {
			out = append(out, uint64(len(strtab)))
			strtab = append(append(strtab, d.val...), 0)
		}
		return out
	}
	dynOffsets, decoyOffsets := offsets(s.dynamic), offsets(s.decoy)
	strSize := cmp.Or(s.strSize, uint64(len(strtab)))
	segment := func(list []dynEntry, at []uint64, flags1 uint64, end bool) []byte {
		var b []byte
		add := func(tag elf.DynTag, v uint64) { b = le.AppendUint64(le.AppendUint64(b, uint64(tag)), v) }
		for i, d := range list {
			add(d.tag, at[i])
		}
		if flags1 != 0 {
			add(elf.DT_FLAGS_1, flags1)
		}
		add(elf.DT_STRTAB, strAt)
		add(elf.DT_STRSZ, strSize)
		if end {
			add(elf.DT_NULL, 0)
		}
		return b
	}
	dyn, decoy := segment(s.dynamic, dynOffsets, s.flags1, !s.noEnd), []byte(nil)
	if s.decoy != nil {
		decoy = segment(s.decoy, decoyOffsets, 0, true)
	}
	names := []byte("\x00.dynstr\x00.dynamic\x00.shstrtab\x00")
	dynAt := strAt + uint64(len(strtab))
	namesAt := dynAt + uint64(len(dyn)) + uint64(len(decoy))
	shoff := namesAt + uint64(len(names))
	total := shoff + 64*4
	var progs []elf.Prog64
	for range twice(s.interp != nil, s.twoInterps) {
		progs = append(progs, elf.Prog64{Type: uint32(elf.PT_INTERP), Off: interpAt, Vaddr: interpAt, Filesz: uint64(len(s.interp)), Memsz: uint64(len(s.interp))})
	}
	for range twice(s.dynamic != nil, s.twoDynamics) {
		size := cmp.Or(s.dynSize, uint64(len(dyn)))
		progs = append(progs, elf.Prog64{Type: uint32(elf.PT_DYNAMIC), Off: dynAt, Vaddr: dynAt, Filesz: size, Memsz: size})
	}
	for i := range twice(!s.noLoad, s.shared) {
		progs = append(progs, elf.Prog64{Type: uint32(elf.PT_LOAD), Vaddr: 16 * uint64(i), Filesz: total, Memsz: total, Align: 4096})
	}
	sectionAt, sectionSize, sectionType := dynAt, uint64(len(dyn)), uint32(elf.SHT_DYNAMIC)
	if decoy != nil {
		sectionAt, sectionSize = dynAt+uint64(len(dyn)), uint64(len(decoy))
	}
	if s.noSection {
		sectionType = uint32(elf.SHT_PROGBITS)
	}
	sections := []elf.Section64{
		{},
		{Name: 1, Type: uint32(elf.SHT_STRTAB), Off: strAt, Addr: strAt, Size: uint64(len(strtab))},
		{Name: 9, Type: sectionType, Off: sectionAt, Addr: sectionAt, Size: sectionSize, Link: 1, Entsize: 16},
		{Name: 18, Type: uint32(elf.SHT_STRTAB), Off: namesAt, Size: uint64(len(names))},
	}
	elfType := elf.ET_DYN
	if s.exec {
		elfType = elf.ET_EXEC
	}
	header := elf.Header64{
		Ident: [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)},
		Type:  uint16(elfType), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Phoff: 64, Shoff: shoff, Ehsize: 64, Phentsize: 56, Phnum: uint16(len(progs)),
		Shentsize: 64, Shnum: uint16(len(sections)), Shstrndx: 3,
	}
	var out bytes.Buffer
	for _, part := range []any{header, progs, slices.Concat(s.interp, strtab, dyn, decoy, names), sections} {
		if err := binary.Write(&out, le, part); err != nil {
			panic(err)
		}
	}
	return out.Bytes()
}

// panics is a file whose every read panics, as a fault in the reader of one would.
type panics struct{}

func (panics) ReadAt([]byte, int64) (int, error) { panic("the read went wrong") }

// TestAnELFFileIsReadForItsLoaderItsLibrariesAndItsSearchLists: readObject answers a file's
// architecture, the loader it names, the libraries it needs in their order, its DT_RPATH and
// DT_RUNPATH split at each colon, nil where it has none, its DT_SONAME, whether it is a library,
// and the first tag of DT_AUDIT, DT_DEPAUDIT, DT_FILTER and DT_AUXILIARY it carries; a tag given twice counts by its last value, as the loaders count it. The entries are
// those at the PT_DYNAMIC segment's address, as its PT_LOAD maps it, whatever a section header
// names. A program with no PT_INTERP names no loader. A program, ET_EXEC or DF_1_PIE, is no
// library. A segment over maxDynamic bytes, a second one, one no PT_LOAD maps, PT_LOAD segments
// that share a page, a segment with no DT_NULL and a string outside its table are read as far as
// the loader, with why the rest is not.
//
// What it costs when this fails: the search follows lists the loader never reads, and grants the
// folders a file's section headers name; a program is taken for a library and its folder
// granted; or the bound on what one file makes the call read is gone.
func TestAnELFFileIsReadForItsLoaderItsLibrariesAndItsSearchLists(t *testing.T) {
	t.Parallel()
	whole := elfSpec{interp: []byte(glibcAMD + "\x00"), dynamic: []dynEntry{
		{elf.DT_NEEDED, "libb.so"}, {elf.DT_RPATH, "/gone"}, {elf.DT_NEEDED, "liba.so"}, {elf.DT_RPATH, "/r1:/r2"},
		{elf.DT_RUNPATH, "$ORIGIN/lib::/x"}, {elf.DT_SONAME, "libtool.so.1"},
	}}
	with := func(change func(*elfSpec)) elfSpec { s := whole; change(&s); return s }
	base := object{class: elf.ELFCLASS64, machine: elf.EM_X86_64}
	full := base
	full.interp, full.soname, full.needed = glibcAMD, "libtool.so.1", []string{"libb.so", "liba.so"}
	full.rpath, full.runpath, full.library = []string{"/r1", "/r2"}, []string{"$ORIGIN/lib", "", "/x"}, true
	program, library := full, base
	program.library, library.library = false, true
	loaderOnly := base
	loaderOnly.interp = glibcAMD
	rows := []struct {
		claim  string
		spec   elfSpec
		want   object
		unread string
	}{
		{"a library with a loader and each list", whole, full, ""},
		{"a program with no loader and no dynamic segment", elfSpec{}, base, ""},
		{"a library with a dynamic segment that names nothing", elfSpec{dynamic: []dynEntry{}}, library, ""},
		{"a dynamic segment with no section header of its type", with(func(s *elfSpec) { s.noSection = true }), full, ""},
		{"a section header that names other entries than the segment", with(func(s *elfSpec) {
			s.decoy = []dynEntry{{elf.DT_NEEDED, "libdecoy.so"}, {elf.DT_RUNPATH, "/"}}
		}), full, ""},
		{"a program, ET_EXEC", with(func(s *elfSpec) { s.exec = true }), program, ""},
		{"a position-independent program, DF_1_PIE", with(func(s *elfSpec) { s.flags1 = uint64(elf.DF_1_PIE | elf.DF_1_NOW) }), program, ""},
		{"a dynamic segment over maxDynamic", with(func(s *elfSpec) { s.dynSize = maxDynamic + 1 }), loaderOnly, "is over " + strconv.Itoa(maxDynamic) + " bytes"},
		{"a second dynamic segment", with(func(s *elfSpec) { s.twoDynamics = true }), loaderOnly, "more than one dynamic segment"},
		{"a dynamic segment no PT_LOAD maps", with(func(s *elfSpec) { s.noLoad = true }), loaderOnly, "its dynamic segment is at an address no PT_LOAD segment maps"},
		{"PT_LOAD segments that share a page", with(func(s *elfSpec) { s.shared = true }), loaderOnly, "share a page"},
		{"a dynamic segment with no DT_NULL", with(func(s *elfSpec) { s.noEnd = true }), loaderOnly, "no DT_NULL within it"},
		{"a string outside its table", with(func(s *elfSpec) { s.strSize = 2 }), loaderOnly, "outside its string table"},
	}
	for _, tag := range unfollowedTags() {
		named := full
		named.unfollowed = tag
		rows = append(rows, struct {
			claim  string
			spec   elfSpec
			want   object
			unread string
		}{"a library that names one by " + tag.String(), with(func(s *elfSpec) {
			s.dynamic = append(slices.Clone(s.dynamic), dynEntry{tag, "/elsewhere/lib.so"}, dynEntry{elf.DT_AUXILIARY, "/elsewhere/lib.so"})
		}), named, ""})
	}
	for _, r := range rows {
		got, err := readObject(bytes.NewReader(r.spec.bytes()), 4096)
		if err != nil {
			t.Errorf("%s: %v", r.claim, err)
			continue
		}
		if says := fmt.Sprint(got.unread); (r.unread == "") != (got.unread == nil) || !strings.Contains(says, r.unread) {
			t.Errorf("%s: its dynamic segment is unread for %q, want %q", r.claim, says, r.unread)
		}
		if got.unread = nil; !reflect.DeepEqual(got, r.want) {
			t.Errorf("%s: read as %+v, want %+v", r.claim, got, r.want)
		}
	}
	if len(rows) < 17 {
		t.Fatalf("ran %d rows", len(rows))
	}
}

// TestAFileThatIsNoELFFileIsAnErrorAndNeverACrash: a #! script is named as one, whose interpreter
// is the program to run; bytes that are no ELF file, a file cut short, a loader's name that is
// empty or over maxInterp bytes, a file that names two loaders, and a read that panics are each an
// error with nothing read.
//
// What it costs when this fails: a file an attacker shaped takes down the caller's process before
// the block is set, a script runs behind folders told for no program, or the search follows a
// loader the kernel does not start.
func TestAFileThatIsNoELFFileIsAnErrorAndNeverACrash(t *testing.T) {
	t.Parallel()
	whole := elfSpec{interp: []byte(glibcAMD + "\x00"), dynamic: []dynEntry{{elf.DT_NEEDED, "libc.so.6"}}}
	file := whole.bytes()
	whole.twoInterps = true
	for _, r := range []struct {
		claim string
		file  io.ReaderAt
		says  string
	}{
		{"a #! script", bytes.NewReader([]byte("#!/bin/sh\nexit 0\n")), "it is a #! script, and its interpreter is the program to run"},
		{"text", bytes.NewReader([]byte("hello\n")), "it is not an ELF file"},
		{"an empty file", bytes.NewReader(nil), "it is not an ELF file"},
		{"an ELF file cut short", bytes.NewReader(file[:len(file)-100]), "it is not an ELF file"},
		{"a loader's name that is empty", bytes.NewReader(elfSpec{interp: []byte{0}}.bytes()), "the name of its loader is empty"},
		{"a loader's name over maxInterp", bytes.NewReader(elfSpec{interp: bytes.Repeat([]byte{'a'}, maxInterp+1)}.bytes()), "or over " + strconv.Itoa(maxInterp) + " bytes"},
		{"two loaders", bytes.NewReader(whole.bytes()), "it names more than one loader, PT_INTERP"},
		{"a read that panics", panics{}, "reading it as an ELF file panicked: the read went wrong"},
	} {
		got, err := readObject(r.file, 4096)
		if err == nil || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v, want an error saying %q", r.claim, err, r.says)
		}
		if !reflect.DeepEqual(got, object{}) {
			t.Errorf("%s: read as %+v with the error, want nothing", r.claim, got)
		}
	}
}

// FuzzReadObject: whatever bytes a file holds, readObject returns: an error with nothing read, or
// a file whose loader's name is within maxInterp bytes and holds no NUL. A panic of debug/elf is
// an error, by readObject's own recover, so the fuzzer finds only what gets past it.
//
// What it costs when this fails: a file shaped to break the ELF reader crashes the process that
// asked to hold a program, where it should be refused.
func FuzzReadObject(f *testing.F) {
	f.Add(elfSpec{interp: []byte(glibcAMD + "\x00"), dynamic: []dynEntry{{elf.DT_NEEDED, "libc.so.6"}, {elf.DT_RUNPATH, "$ORIGIN/../lib"}}}.bytes())
	f.Add(elfSpec{dynamic: []dynEntry{{elf.DT_SONAME, "libx.so"}}, noSection: true, flags1: uint64(elf.DF_1_PIE)}.bytes())
	f.Add(elfSpec{dynamic: []dynEntry{{elf.DT_NEEDED, "libx.so"}}, decoy: []dynEntry{{elf.DT_NEEDED, "liby.so"}}, shared: true}.bytes())
	f.Add(elfSpec{interp: []byte{0}, dynSize: maxDynamic + 1, twoInterps: true}.bytes())
	f.Add([]byte("#!/bin/sh\n"))
	f.Add([]byte("\x7fELF"))
	f.Fuzz(func(t *testing.T, file []byte) {
		got, err := readObject(bytes.NewReader(file), 4096)
		if err != nil && !reflect.DeepEqual(got, object{}) {
			t.Errorf("read as %+v with the error %v, want nothing", got, err)
		}
		if len(got.interp) > maxInterp || strings.Contains(got.interp, "\x00") || err == nil && bytes.HasPrefix(file, []byte("#!")) {
			t.Errorf("read as %+v, %v: a loader's name past the bound or with a NUL, or a script taken", got, err)
		}
	})
}
