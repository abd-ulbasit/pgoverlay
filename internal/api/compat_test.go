package api

import (
	"encoding/json"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

// The /v1 REST surface is a backward-compatibility promise: fields may be
// ADDED, but documented fields are not renamed or removed without a /v2 (see
// docs/api.md). These tests lock the documented JSON keys of each response
// type — adding a field is fine, renaming/removing one fails here.
func wireKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not a JSON object: %s", b)
	}
	got := map[string]bool{}
	for k := range m {
		got[k] = true
	}
	return got
}

func assertKeys(t *testing.T, label string, v any, want ...string) {
	t.Helper()
	got := wireKeys(t, v)
	for _, k := range want {
		if !got[k] {
			t.Errorf("%s: missing documented JSON field %q (renamed or removed? that's a /v1 break)", label, k)
		}
	}
}

func TestV1WireCompat(t *testing.T) {
	// fully-populated instances so omitempty fields are present too
	assertKeys(t, "Branch", Branch{
		Name: "pr-1", Source: "main", Parent: "p", State: "ready", Host: "h", Port: 5432,
		User: "postgres", Password: "x", Database: "postgres", ProxyDatabase: "postgres@pr-1",
		ExpiresAt: "t", CreatedAt: "t",
	}, "name", "source", "parent", "state", "host", "port", "user", "password",
		"database", "proxy_database", "expires_at", "created_at")

	assertKeys(t, "Source", Source{
		Name: "main", PGVersion: "17", Host: "h", Port: 5432, User: "u", Database: "d",
		Network: "n", Via: "basebackup", DumpSchemas: []string{"public"}, State: "ready",
		Generation: 1, CreatedAt: "t",
	}, "name", "pg_version", "host", "port", "user", "database", "network", "via",
		"dump_schemas", "state", "generation", "created_at")

	assertKeys(t, "Token", Token{Name: "ci", Role: "operator", CreatedAt: "t"},
		"name", "role", "created_at")

	assertKeys(t, "DiffResult", engine.DiffResult{
		SchemaDiff: "diff", Tables: []engine.TableDelta{{Table: "t"}},
	}, "schema_diff", "tables")
	assertKeys(t, "TableDelta", engine.TableDelta{
		Table: "t", BaseRows: 1, BranchRows: 2, Delta: 1, SampleRows: []map[string]any{{"id": 1}},
	}, "table", "base_rows", "branch_rows", "delta", "sample_rows")

	assertKeys(t, "ReconcilePlan", engine.ReconcilePlan{
		Actions: []engine.Action{{Kind: "gc_volume", Target: "v", Reason: "r"}},
	}, "actions")
	assertKeys(t, "Action", engine.Action{Kind: "gc_volume", Target: "v", Reason: "r"},
		"kind", "target", "reason")
}
