package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/cow"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl/pgctltest"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// The lazyrw integration on a real kernel and image: a branch the engine
// creates starts with the shim active (cow-mode lazyrw), a read-only query
// on a frozen table adds next to nothing to its writable layer while a
// --lazyrw=off branch copies the table, and a branch-from-branch child and
// its restarted parent come up with the shim as well. On arm64 runners this
// exercises the aarch64 build (CI job integration-arm64 runs Cow tests).
// The full copy-on-write torture suite is cow_it_test.go.
func TestCowLazyRWBranchReadsCopyNothing(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctltest.StartSourcePG(t, ctx)
	// ~40 MB, frozen and checkpointed so a branch's reads set no hint bits
	mustExec(t, ctx, hostConn, `CREATE TABLE big AS SELECT g AS id, repeat('x', 300) AS pad FROM generate_series(1, 120000) g`)
	mustExec(t, ctx, hostConn, `VACUUM (FREEZE, ANALYZE) big`)
	mustExec(t, ctx, hostConn, `CHECKPOINT`)
	tableBytes := int64(mustQueryInt(t, ctx, hostConn, `SELECT pg_relation_size('big')::int`))

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, d, "postgres:17")
	eOff := New(r, d, "postgres:17", WithLazyRW(false)) // same registry, --lazyrw=off

	src := &registry.Source{Name: "cow-lrw-src", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })
	create := func(eng *Engine, name string, from func() (*registry.Branch, error)) *registry.Branch {
		t.Helper()
		b, err := from()
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			if err := eng.DestroyBranch(context.Background(), name); err != nil {
				t.Errorf("destroy %s: %v", name, err)
			}
		})
		return b
	}
	mode := func(b *registry.Branch) string {
		t.Helper()
		b, err := r.GetBranchByName(b.Name)
		if err != nil {
			t.Fatal(err)
		}
		out, err := d.ExecOutput(ctx, b.ContainerID, []string{"cat", cow.CowModePath})
		if err != nil {
			t.Fatalf("read cow-mode of %s: %v", b.Name, err)
		}
		return out
	}
	// readGrowth is how much a read-only pass over big grows the branch's
	// writable layer, both sides measured after a CHECKPOINT
	readGrowth := func(eng *Engine, b *registry.Branch) int64 {
		t.Helper()
		b, err := r.GetBranchByName(b.Name)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, branchConn(b), `CHECKPOINT`)
		before, err := eng.BranchUsage(ctx, b.Name)
		if err != nil {
			t.Fatal(err)
		}
		if n := mustQueryInt(t, ctx, branchConn(b), `SELECT count(*) FROM big`); n != 120000 {
			t.Fatalf("%s: count = %d", b.Name, n)
		}
		mustQueryInt(t, ctx, branchConn(b), `SELECT pg_relation_size('big')::int`)
		mustExec(t, ctx, branchConn(b), `CHECKPOINT`)
		after, err := eng.BranchUsage(ctx, b.Name)
		if err != nil {
			t.Fatal(err)
		}
		return after - before
	}

	b1 := create(e, "cow-lrw-1", func() (*registry.Branch, error) { return e.CreateBranch(ctx, "cow-lrw-1", src.Name, 0) })
	if m := mode(b1); !strings.HasPrefix(m, cow.CowModeLazyRW+"\n") {
		t.Fatalf("cow-lrw-1 cow-mode = %q, want lazyrw", m)
	}
	if n := e.CowModeCounts()[cow.CowModeLazyRW]; n != 1 {
		t.Errorf("CowModeCounts[lazyrw] = %d, want 1", n)
	}
	if g := readGrowth(e, b1); g >= 2<<20 {
		t.Errorf("a read of the %d-byte table grew the lazyrw branch by %d bytes, want < 2 MiB", tableBytes, g)
	} else {
		t.Logf("lazyrw: a read of the %d-byte table grew the branch by %d bytes", tableBytes, g)
	}

	off := create(eOff, "cow-lrw-off", func() (*registry.Branch, error) { return eOff.CreateBranch(ctx, "cow-lrw-off", src.Name, 0) })
	if m := mode(off); !strings.HasPrefix(m, cow.CowModeOff+"\n") {
		t.Fatalf("cow-lrw-off cow-mode = %q, want off", m)
	}
	if g := readGrowth(eOff, off); g < tableBytes/2 {
		t.Errorf("with lazyrw off a read grew the branch by only %d bytes; the %d-byte table should have been copied", g, tableBytes)
	} else {
		t.Logf("lazyrw off: the same read grew the branch by %d bytes", g)
	}

	// branch-from-branch: the parent restarts on a fresh rw volume, the child
	// stacks on the frozen one; both run the shim and read without copying
	mustExec(t, ctx, branchConn(b1), `UPDATE big SET pad = 'parent' WHERE id = 1`)
	child := create(e, "cow-lrw-2", func() (*registry.Branch, error) { return e.CreateBranchFrom(ctx, "cow-lrw-2", "cow-lrw-1", 0) })
	for _, b := range []*registry.Branch{b1, child} {
		if m := mode(b); !strings.HasPrefix(m, cow.CowModeLazyRW+"\n") {
			t.Errorf("%s cow-mode after the freeze = %q, want lazyrw", b.Name, m)
		}
	}
	if n := mustQueryInt(t, ctx, branchConn(child), `SELECT count(*) FROM big WHERE id = 1 AND pad = 'parent'`); n != 1 {
		t.Fatal("the child does not see the parent's write")
	}
	if g := readGrowth(e, child); g >= 2<<20 {
		t.Errorf("a read in the child grew it by %d bytes, want < 2 MiB", g)
	}
}
