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
