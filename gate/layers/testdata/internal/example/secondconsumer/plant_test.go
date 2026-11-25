package secondconsumer_test

import (
	_ "github.com/birdtable/go-proc/gitenv"
	_ "github.com/birdtable/go-proc/gitenv/gittest"
	_ "github.com/birdtable/go-proc/internal/example/secondconsumer"
	_ "github.com/birdtable/go-proc/internal/homevars" // want layers-internal/example/secondconsumer-tests
	_ "github.com/birdtable/go-proc/load"
	_ "github.com/birdtable/go-proc/proc"
	_ "github.com/birdtable/go-proc/sandbox"
	_ "github.com/birdtable/go-proc/service"
)
