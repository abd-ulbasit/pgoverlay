package cli

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/config"
)

// Local mode reads $PGOVERLAY_LAZYRW and $PGOVERLAY_WAL_RECYCLE as branchd
// does, and refuses a value that is not on or off before touching Docker.
func TestCowOptionsFromEnvironment(t *testing.T) {
	for _, cfg := range []config.Config{{}, {LazyRW: "off"}, {LazyRW: "on", WALRecycle: "off"}} {
		if opts, err := cowOptions(&cfg); err != nil || len(opts) != 2 {
			t.Errorf("%+v: %d options, err %v", cfg, len(opts), err)
		}
	}
	if _, err := cowOptions(&config.Config{LazyRW: "true"}); err == nil || !strings.Contains(err.Error(), "$PGOVERLAY_LAZYRW") {
		t.Errorf("LazyRW=true: err %v, want one naming $PGOVERLAY_LAZYRW", err)
	}
	if _, err := cowOptions(&config.Config{WALRecycle: "no"}); err == nil || !strings.Contains(err.Error(), "$PGOVERLAY_WAL_RECYCLE") {
		t.Errorf("WALRecycle=no: err %v, want one naming $PGOVERLAY_WAL_RECYCLE", err)
	}
}
