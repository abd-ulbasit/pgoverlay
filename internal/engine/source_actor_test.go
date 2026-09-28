package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// SECRETS-03/06: AddSource, RefreshSource and RemoveSource journal the actor
// on their context, not the daemon.
func TestSourceOpsRecordRequestActor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	r, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := New(r, newFake(), "postgres:17")
	ctx := registry.WithActor(context.Background(), registry.Actor{Name: "alice", Role: registry.RoleAdmin})

	if err := e.AddSource(ctx, &registry.Source{Name: "main", PGVersion: "17", ConnHost: "db"}, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshSource(ctx, "main", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveSource(ctx, "main"); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT to_state, actor FROM transitions WHERE entity='source' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var to, actor string
		if err := rows.Scan(&to, &actor); err != nil {
			t.Fatal(err)
		}
		if actor != "alice (admin)" {
			t.Errorf("source -> %s journaled as %q, want alice (admin)", to, actor)
		}
		states = append(states, to)
	}
	if got := strings.Join(states, ","); got != "seeding,ready,ready,deleted" {
		t.Fatalf("source journal %s, want seeding,ready,ready,deleted", got)
	}
}
