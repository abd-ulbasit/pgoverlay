package engine

import (
	"context"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// Regression for #6: create p -> branch c from p -> destroy p -> create p.
// The freeze turned p's first rw volume into the layer c mounts read-only;
// the recreated p must get a volume no row has used, never that layer (or any
// other earlier volume of the name), and destroying the new p must leave c's
// layer alone.
func TestRecreatedBranchNeverAdoptsFrozenLayer(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgoverlay-src-main"] = true
	ctx := context.Background()

	if _, err := e.CreateBranch(ctx, "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	c, err := e.CreateBranchFrom(ctx, "c", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := r.LayerChain(c.ID)
	if err != nil || len(chain) != 1 {
		t.Fatalf("child chain=%v err=%v", chain, err)
	}
	layer := chain[0].Volume
	oldP, _ := r.GetBranchByName("p")
	if err := e.DestroyBranch(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if !d.volumes[layer] {
		t.Fatalf("destroying p removed c's layer %q", layer)
	}

	existing := map[string]bool{}
	for v := range d.volumes {
		existing[v] = true
	}
	p2, err := e.CreateBranch(ctx, "p", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p2.RWVolume == layer || p2.RWVolume == oldP.RWVolume {
		t.Fatalf("recreated p reuses an earlier volume: rw=%q (c's layer %q, old p rw %q)", p2.RWVolume, layer, oldP.RWVolume)
	}
	if existing[p2.RWVolume] {
		t.Fatalf("recreated p's rw volume %q already existed (would be adopted, not created)", p2.RWVolume)
	}
	if got := mountAt(t, d.branches[len(d.branches)-1], "/pgoverlay/rw"); got.Volume != p2.RWVolume {
		t.Fatalf("new p container mounts %q at rw, want %q", got.Volume, p2.RWVolume)
	}

	// destroying the new p leaves c's layer (and c) intact
	if err := e.DestroyBranch(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if !d.volumes[layer] {
		t.Fatalf("destroying the recreated p removed c's layer %q", layer)
	}
	if got, err := r.GetBranchByName("c"); err != nil || got.State != registry.BranchReady {
		t.Fatalf("child after parent churn: %+v err=%v", got, err)
	}
}

// A recreated parent that already sits on a later generation of its name
// must not freeze into its own rw volume: the swap volume is also a fresh
// generation.
func TestFreezeOfRecreatedParentPicksFreshSwapVolume(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	readySource(t, r)
	d.volumes["pgoverlay-src-main"] = true
	ctx := context.Background()

	if _, err := e.CreateBranch(ctx, "p", "main", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(ctx, "c", "p", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.DestroyBranch(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	p2, err := e.CreateBranch(ctx, "p", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(ctx, "c2", "p", 0); err != nil {
		t.Fatal(err)
	}
	p3, _ := r.GetBranchByName("p")
	if p3.RWVolume == p2.RWVolume {
		t.Fatalf("freeze swapped p onto its own frozen volume %q", p2.RWVolume)
	}
	c2, _ := r.GetBranchByName("c2")
	chain, _ := r.LayerChain(c2.ID)
	if len(chain) != 1 || chain[0].Volume != p2.RWVolume {
		t.Fatalf("c2 chain=%v want [%s]", chain, p2.RWVolume)
	}
	// every volume name in play is distinct
	seen := map[string]string{}
	for _, name := range []string{"c", "c2", "p"} {
		b, _ := r.GetBranchByName(name)
		if other, dup := seen[b.RWVolume]; dup {
			t.Fatalf("%s and %s share rw volume %q", name, other, b.RWVolume)
		}
		seen[b.RWVolume] = name
	}
}
