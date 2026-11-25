# gittest

The git a test runs: a clean environment, the host's git, and a repository of its own.

[![Go Reference](https://pkg.go.dev/badge/github.com/birdtable/go-proc/gitenv/gittest.svg)](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv/gittest)

`gittest` is for tests only. Built on `gitenv` and go-testkit's `Clean`, it gives a git child no
`GIT_` name but its own, no home of the person's, no prompt and an identity to commit under, so a
test never reads your configuration or your keys. You give it the folder and the files to commit.

```
go get github.com/birdtable/go-proc/gitenv/gittest
```

```go
func TestStatus(t *testing.T) {
	dir := gittest.Repo(t, map[string]string{"a.txt": "hello\n"}, nil)
	cmd := exec.Command(gittest.GitPath(t), "-C", dir, "status", "--porcelain")
	cmd.Env = gittest.Git(t.TempDir())
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("out %q, err %v", out, err)
	}
}
```

## What's inside

- `Git(dir)`: the environment of a git child, with `dir` as its home and its global configuration.
- `GitPath(t)`: the absolute path of the host's git. A host with none fails the test.
- `Repo(t, files, links)`: a repository in a temp folder of the test's own, with the files and
  symbolic links committed once on git's default branch.

## Systems

Linux, macOS and Windows. `Git` also sets a Kerberos ticket cache that holds nothing, so a test
never authenticates as the person.

## Reference

Every exported name is documented at
[pkg.go.dev/github.com/birdtable/go-proc/gitenv/gittest](https://pkg.go.dev/github.com/birdtable/go-proc/gitenv/gittest).
