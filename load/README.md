# load

Read the system's load average, and refuse where there is none.

[![Go Reference](https://pkg.go.dev/badge/github.com/birdtable/go-proc/load.svg)](https://pkg.go.dev/github.com/birdtable/go-proc/load)

`load` gives you the one, five and fifteen minute load averages and the number of CPUs, and nothing
more. It sets no threshold. You decide what a figure is too high for. It starts no program.

```
go get github.com/birdtable/go-proc/load
```

```go
r, err := load.Read()
if errors.Is(err, load.ErrNoAverage) {
	return // this system keeps none: wait as usual
}
if err != nil {
	return err
}
if r.One > float64(r.CPUs) {
	time.Sleep(time.Second) // busy: give it a moment
}
```

## What's inside

- `Read()`: a `Reading` with `One`, `Five`, `Fifteen` and `CPUs`.
- `ErrNoAverage`: every refusal matches it, and it matches `errors.ErrUnsupported`.

`Read` never returns a zero `Reading` in place of an error.

## Systems

- **macOS:** reads the `vm.loadavg` sysctl.
- **Linux:** reads `/proc/loadavg` (and `sysinfo(2)` if it cannot) through gopsutil.
- **Windows and everything else:** returns `ErrNoAverage`. Windows keeps no load average.

The figures are not the same load everywhere: macOS counts threads waiting for a CPU, and Linux
counts those in uninterruptible sleep too, as on a disk.

## Reference

Every exported name is documented at
[pkg.go.dev/github.com/birdtable/go-proc/load](https://pkg.go.dev/github.com/birdtable/go-proc/load).
