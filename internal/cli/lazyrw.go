package cli

import (
	"fmt"

	"github.com/abd-ulbasit/pgoverlay/internal/config"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// cowOptions turns $PGOVERLAY_LAZYRW and $PGOVERLAY_WAL_RECYCLE into engine
// options, so local mode starts branches as a branchd with the same
// environment would (there they are the defaults of --lazyrw and
// --wal-recycle).
func cowOptions(cfg *config.Config) ([]engine.Option, error) {
	lazyrw, err := config.ParseOnOff(cfg.LazyRW, true)
	if err != nil {
		return nil, fmt.Errorf("$%s: %w", config.LazyRWEnv, err)
	}
	walRecycle, err := config.ParseOnOff(cfg.WALRecycle, true)
	if err != nil {
		return nil, fmt.Errorf("$%s: %w", config.WALRecycleEnv, err)
	}
	return []engine.Option{engine.WithLazyRW(lazyrw), engine.WithWALRecycle(walRecycle)}, nil
}
