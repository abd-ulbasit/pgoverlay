package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// TestAddSourceRejectsUnusableConnection covers `pgb source add --host ""`
// (and its API equivalent): an empty host made pg_basebackup fall back to a
// Unix socket inside the helper container and fail with a confusing error,
// after a source row and layer had been created. AddSource now refuses such
// settings up front.
func TestAddSourceRejectsUnusableConnection(t *testing.T) {
	cases := map[string]*registry.Source{
		"empty host":   {Name: "main", PGVersion: "17", ConnHost: "", ConnPort: 5432, ConnUser: "postgres"},
		"blank host":   {Name: "main", PGVersion: "17", ConnHost: "  ", ConnPort: 5432, ConnUser: "postgres"},
		"bad port":     {Name: "main", PGVersion: "17", ConnHost: "db", ConnPort: 0, ConnUser: "postgres"},
		"empty user":   {Name: "main", PGVersion: "17", ConnHost: "db", ConnPort: 5432, ConnUser: ""},
		"comma schema": {Name: "main", PGVersion: "17", ConnHost: "db", ConnPort: 5432, ConnUser: "postgres", SeedVia: registry.SeedViaDump, DumpSchemas: []string{`"a,b"`}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			d := newFake()
			e, r := testEngine(t, d)
			err := e.AddSource(context.Background(), s, "secret")
			if !errors.Is(err, pgctl.ErrInvalidSpec) {
				t.Fatalf("AddSource err = %v, want pgctl.ErrInvalidSpec", err)
			}
			if _, gerr := r.GetSourceByName("main"); gerr == nil {
				t.Fatal("AddSource created a source row for an unusable connection")
			}
			if len(d.helpers) != 0 {
				t.Fatalf("AddSource ran %d helpers for an unusable connection", len(d.helpers))
			}
		})
	}
}

// A basebackup of a PG 15 server into the default postgres:17 image used to
// mark the source ready; every branch then died on "database files are
// incompatible with server". The seed now fails, the layer is removed and
// the source is marked failed with the reason.
func TestAddSourceFailsOnMajorVersionMismatch(t *testing.T) {
	d := newFake()
	d.helperOut = "pgoverlay-data-version=15\npgoverlay-server-version=postgres (PostgreSQL) 17.6\n"
	e, r := testEngine(t, d)
	s := &registry.Source{Name: "main", PGVersion: "17", ConnHost: "db", ConnPort: 5432, ConnUser: "postgres"}
	err := e.AddSource(context.Background(), s, "secret")
	if !errors.Is(err, pgctl.ErrVersionMismatch) {
		t.Fatalf("AddSource err = %v, want pgctl.ErrVersionMismatch", err)
	}
	got, gerr := r.GetSourceByName("main")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.State != registry.SourceFailed {
		t.Fatalf("source state = %s, want failed", got.State)
	}
	if d.volumes[got.Volume] {
		t.Fatalf("source layer %s kept after the failed seed", got.Volume)
	}
}
