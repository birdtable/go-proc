# Contributing

Thanks for helping. Issues and pull requests are both welcome.

## Issues

- **A bug:** say the go-proc version, the Go version, the system, and give the shortest program
  that shows it.
- **A feature:** say what you're trying to do, not only the API you'd like. A need can often be
  met another way.
- **A security problem:** please don't open an issue. See [SECURITY.md](SECURITY.md).

## Running the tests

You need Go (the version in `go.mod`). Every other tool is pinned in `tools/<tool>.mod` and built
by `go tool` on first use, so nothing is installed on your machine.

```sh
git clone https://github.com/birdtable/go-proc && cd go-proc
go tool -modfile=tools/task.mod task hooks      # installs the git hooks
go tool -modfile=tools/task.mod task pre-push   # build, lint, short tests, API check
```

`go tool -modfile=tools/task.mod task` lists every task. The three tiers:

- `pre-commit` runs on each commit: formatting, vet, lint and the fastest tests.
- `pre-push` runs on each push: the build for every system, the short tests and `task api`,
  which holds the exported names to `api/`.
- `release` runs every test and every check, the module audit and licences among them.

On Linux the tiers also run `proc`'s tests in a user and pid namespace of their own
(`task namespace`). That needs `unshare` (from util-linux), `tini`, and a kernel that lets your
user make a user namespace.

## Pull requests

- Keep a pull request to one change, and say in its description what it changes and why.
- Run `task pre-push` before you push. The hooks run it for you once `task hooks` is set up.
- A change a user would notice gets a changelog entry: `go tool -modfile=tools/changie.mod changie new`.
- A new exported name needs a doc comment and a test, and a refusal needs a test that it refuses.
- Every exported default must be safe, documented and overridable: a value only a caller can know
  comes from the caller.
- Write commit messages as a short summary line, then a blank line and the why.

By contributing, you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
