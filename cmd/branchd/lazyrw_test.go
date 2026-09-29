package main

import (
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
)

// --lazyrw and --wal-recycle take on or off; anything else stops branchd at
// startup, naming the flag and its environment variable.
func TestLazyRWOptions(t *testing.T) {
	for _, backend := range []cow.Backend{cow.BackendOverlay, cow.BackendZFS, cow.BackendCSI} {
		for _, c := range [][2]string{{"on", "on"}, {"off", "on"}, {"on", "off"}, {"off", "off"}} {
			opts, err := lazyrwOptions(c[0], c[1], backend)
			if err != nil || len(opts) != 2 {
				t.Errorf("%s --lazyrw=%s --wal-recycle=%s: %d options, err %v", backend, c[0], c[1], len(opts), err)
			}
		}
	}
	for _, c := range []struct{ lazyrw, walRecycle, want string }{
		{"yes", "on", "--lazyrw (or $PGOVERLAY_LAZYRW)"},
		{"", "on", ""},
		{"on", "0", "--wal-recycle (or $PGOVERLAY_WAL_RECYCLE)"},
	} {
		_, err := lazyrwOptions(c.lazyrw, c.walRecycle, cow.BackendOverlay)
		if c.want == "" {
			if err != nil {
				t.Errorf("--lazyrw=%q --wal-recycle=%q: %v", c.lazyrw, c.walRecycle, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("--lazyrw=%q --wal-recycle=%q: err %v, want one naming %s", c.lazyrw, c.walRecycle, err, c.want)
		}
	}
}
