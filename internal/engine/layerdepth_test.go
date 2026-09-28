package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// LIFECYCLE-10: every fork of an overlay parent stacks one more frozen layer
// on it. Past --max-layer-depth the fork is refused up front (quota error, no
// row, no freeze) instead of failing later at mount time and taking the
// parent down with it.
func TestBranchFromRefusedPastMaxLayerDepth(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithMaxLayerDepth(2))
	readySource(t, r)
	ctx := context.Background()
	if _, err := e.CreateBranch(ctx, "fixture", "main", 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"t-1", "t-2"} {
		if _, err := e.CreateBranchFrom(ctx, name, "fixture", 0); err != nil {
			t.Fatalf("fork %s: %v", name, err)
		}
	}
	starts := d.startAttempts
	for _, tc := range []struct{ child, parent string }{{"t-3", "fixture"}, {"t-2-a", "t-2"}} {
		_, err := e.CreateBranchFrom(ctx, tc.child, tc.parent, 0)
		if !errors.Is(err, ErrQuotaExceeded) || !strings.Contains(err.Error(), "--max-layer-depth=2") {
			t.Fatalf("fork %s from %s past the depth limit: err=%v, want quota refusal", tc.child, tc.parent, err)
		}
		if _, err := r.GetBranchByName(tc.child); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("refused fork left a row: %v", err)
		}
	}
	if d.startAttempts != starts {
		t.Fatal("refused fork touched the parent (freeze started)")
	}
	if p, _ := r.GetBranchByName("fixture"); p.State != registry.BranchReady {
		t.Fatalf("parent state=%s", p.State)
	}
}

func TestDefaultMaxLayerDepth(t *testing.T) {
	d := newFake()
	e, _ := testEngine(t, d)
	chain := make([]registry.Layer, DefaultMaxLayerDepth-1)
	p := &registry.Branch{Name: "p"}
	if err := e.checkLayerDepth(p, chain); err != nil {
		t.Fatalf("depth %d: %v", len(chain), err)
	}
	if err := e.checkLayerDepth(p, append(chain, registry.Layer{})); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("depth %d: err=%v want quota refusal", DefaultMaxLayerDepth, err)
	}
}
