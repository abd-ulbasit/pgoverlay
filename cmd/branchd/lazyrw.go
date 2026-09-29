package main

import (
	"fmt"
	"log"

	"github.com/abd-ulbasit/pgoverlay/internal/config"
	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// lazyrwOptions resolves --lazyrw and --wal-recycle (on|off each) into engine
// options and logs what overlay branches will do. Both only affect the
// overlay backend; with zfs or csi they are accepted and ignored.
func lazyrwOptions(lazyrw, walRecycle string, backend cow.Backend) ([]engine.Option, error) {
	lazyrwOn, err := config.ParseOnOff(lazyrw, true)
	if err != nil {
		return nil, fmt.Errorf("--lazyrw (or $%s): %w", config.LazyRWEnv, err)
	}
	walRecycleOn, err := config.ParseOnOff(walRecycle, true)
	if err != nil {
		return nil, fmt.Errorf("--wal-recycle (or $%s): %w", config.WALRecycleEnv, err)
	}
	if backend == cow.BackendOverlay {
		if lazyrwOn {
			log.Printf("--lazyrw=on: overlay branches copy a relation file into their writable layer on its first write, not when it is read (each branch checks its kernel and image at start and falls back to copying on open, with a warning)")
		} else {
			log.Printf("--lazyrw=off: overlay branches copy every relation file Postgres opens into their writable layer, reads included")
		}
		if !walRecycleOn {
			log.Printf("--wal-recycle=off: overlay branches start with wal_recycle=off (experimental)")
		}
	}
	return []engine.Option{engine.WithLazyRW(lazyrwOn), engine.WithWALRecycle(walRecycleOn)}, nil
}
