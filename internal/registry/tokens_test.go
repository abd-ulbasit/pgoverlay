package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCreateAPITokenReturnsPlaintextAndStoresOnlyHash(t *testing.T) {
	r := openTest(t)
	plaintext, err := r.CreateAPIToken("ci", RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) != 32 {
		t.Fatalf("token len=%d want 32 hex chars", len(plaintext))
	}
	// the plaintext must never be stored: only its sha256 hex lives in the row.
	var stored string
	if err := r.db.QueryRow(`SELECT token_hash FROM api_tokens WHERE name='ci'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == plaintext {
		t.Fatal("plaintext token stored in token_hash column")
	}
	want := sha256.Sum256([]byte(plaintext))
	if stored != hex.EncodeToString(want[:]) {
		t.Fatalf("token_hash=%q want sha256 hex of plaintext", stored)
	}
}

func TestLookupAPIToken(t *testing.T) {
	r := openTest(t)
	plaintext, err := r.CreateAPIToken("viewer-1", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	role, ok := r.LookupAPIToken(plaintext)
	if !ok || role != RoleViewer {
		t.Fatalf("LookupAPIToken=%q,%v want viewer,true", role, ok)
	}
	if _, ok := r.LookupAPIToken("deadbeefdeadbeefdeadbeefdeadbeef"); ok {
		t.Fatal("LookupAPIToken matched an unknown token")
	}
	if _, ok := r.LookupAPIToken(""); ok {
		t.Fatal("LookupAPIToken matched the empty token")
	}
}

// TestLookupAPITokenIndexedPath exercises the v10 indexed point-lookup path:
// a known token resolves to its role, an unknown token is rejected, all via
// the WHERE token_hash=? query (no scan-all).
func TestLookupAPITokenIndexedPath(t *testing.T) {
	r := openTest(t)
	plaintext, err := r.CreateAPIToken("op-1", RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := r.LookupAPIToken(plaintext); !ok || role != RoleOperator {
		t.Fatalf("LookupAPIToken(known) = %q,%v want operator,true", role, ok)
	}
	if role, ok := r.LookupAPIToken("ffffffffffffffffffffffffffffffff"); ok {
		t.Fatalf("LookupAPIToken(unknown) = %q,%v want \"\",false", role, ok)
	}
}

// TestMigrationV10IndexExists asserts a fresh Open lands at user_version 10 and
// the unique index on api_tokens(token_hash) exists.
func TestMigrationV10IndexExists(t *testing.T) {
	r := openTest(t)
	var version int
	if err := r.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 10 {
		t.Fatalf("user_version = %d, want >= 10 (the api_tokens_hash index migration)", version)
	}
	var name string
	err := r.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='api_tokens_hash'`).Scan(&name)
	if err != nil {
		t.Fatalf("api_tokens_hash index not found: %v", err)
	}
}

func TestListAndRevokeAPIToken(t *testing.T) {
	r := openTest(t)
	if _, err := r.CreateAPIToken("a", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAPIToken("b", RoleViewer); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListAPITokens len=%d want 2", len(list))
	}
	for _, tok := range list {
		if tok.Name == "" || tok.Role == "" || tok.CreatedAt == "" {
			t.Fatalf("token metadata incomplete: %+v", tok)
		}
	}
	if err := r.RevokeAPIToken("a"); err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListAPITokens()
	if len(list) != 1 || list[0].Name != "b" {
		t.Fatalf("after revoke list=%+v want only b", list)
	}
	if err := r.RevokeAPIToken("missing"); err != ErrNotFound {
		t.Fatalf("RevokeAPIToken(missing)=%v want ErrNotFound", err)
	}
}

func TestCreateAPITokenRejectsBadRoleAndDupName(t *testing.T) {
	r := openTest(t)
	if _, err := r.CreateAPIToken("x", "superuser"); err == nil {
		t.Fatal("CreateAPIToken accepted an invalid role")
	}
	if _, err := r.CreateAPIToken("dup", RoleViewer); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAPIToken("dup", RoleAdmin); err == nil {
		t.Fatal("CreateAPIToken accepted a duplicate name")
	}
}
