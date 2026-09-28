package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// LIFECYCLE-04: a source can name its own image (extensions, locales, libc
// matching the source server). Seed helpers and every branch container of
// that source run it; other sources keep postgres:<pg_version>.
func TestSourceImageOverrideUsedForSeedAndBranches(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	ctx := context.Background()
	s := &registry.Source{Name: "geo", PGVersion: "17", ConnHost: "h", ConnPort: 5432, ConnUser: "postgres",
		Image: "postgis/postgis:17-3.5"}
	if err := e.AddSource(ctx, s, "pw"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetSourceByName("geo"); got.Image != "postgis/postgis:17-3.5" {
		t.Fatalf("image not stored: %+v", got)
	}
	for _, h := range d.helpers {
		if len(h.Cmd) > 0 && h.Cmd[0] == "pg_basebackup" && h.Image != "postgis/postgis:17-3.5" {
			t.Fatalf("seed helper image=%q", h.Image)
		}
	}
	if _, err := e.CreateBranch(ctx, "p", "geo", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranchFrom(ctx, "c", "p", 0); err != nil {
		t.Fatal(err)
	}
	if len(d.branches) != 3 { // p, p restarted by the freeze, c
		t.Fatalf("branch starts=%d", len(d.branches))
	}
	for _, spec := range d.branches {
		if spec.Image != "postgis/postgis:17-3.5" {
			t.Fatalf("branch %s image=%q, want the source's image", spec.Name, spec.Image)
		}
	}

	// a source without an override keeps the default image
	plain := &registry.Source{Name: "plain", PGVersion: "16", ConnHost: "h", ConnPort: 5432, ConnUser: "postgres"}
	if err := e.AddSource(ctx, plain, "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranch(ctx, "q", "plain", 0); err != nil {
		t.Fatal(err)
	}
	if got := d.branches[len(d.branches)-1].Image; got != "postgres:16" {
		t.Fatalf("default branch image=%q want postgres:16", got)
	}
}

func TestSourceImageRejectsGarbage(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d)
	for _, img := range []string{"postgis/postgis:17 ; rm -rf /", "Upper/Case:1", "has space:1", "img:tag\nkey: value"} {
		err := e.AddSource(context.Background(), &registry.Source{Name: "x", PGVersion: "17", Image: img, ConnHost: "h", ConnPort: 5432, ConnUser: "postgres"}, "pw")
		if !errors.Is(err, registry.ErrInvalidImage) {
			t.Errorf("image %q: err=%v, want ErrInvalidImage", img, err)
		}
	}
	if list, _ := r.ListSources(); len(list) != 0 {
		t.Fatalf("rejected images left rows: %+v", list)
	}
	for _, img := range []string{"postgres:17", "ghcr.io/acme/postgres:17-pgvector", "localhost:5000/pg/custom", "pgvector/pgvector:pg17@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if err := e.AddSource(context.Background(), &registry.Source{Name: "ok", PGVersion: "17", Image: img, ConnHost: "h", ConnPort: 5432, ConnUser: "postgres"}, "pw"); err != nil {
			t.Errorf("image %q rejected: %v", img, err)
		}
		if err := e.RemoveSource(context.Background(), "ok"); err != nil {
			t.Fatal(err)
		}
	}
}
