package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// Token rotation and at-rest key changes, end to end through the REST API.
// Each "restart" closes the registry and serves the same registry file with a
// new admin token and/or at-rest keys, wired the way branchd wires them
// (dedicated primary key + sha256(PGOVERLAY_TOKEN) as the legacy fallback,
// then a re-encryption sweep).

const (
	tokenA = "admin-token-aaaaaaaa"
	tokenB = "admin-token-bbbbbbbb"
)

type restartable struct {
	t    *testing.T
	path string
	d    *fakeDriver
	reg  *registry.Registry
	ts   *httptest.Server
}

func newRestartable(t *testing.T) *restartable {
	return &restartable{t: t, path: filepath.Join(t.TempDir(), "r.db"), d: newFake()}
}

// start (re)serves the registry file with the given admin token and primary
// key, rotation on.
func (s *restartable) start(adminToken string, primary []byte) {
	s.t.Helper()
	s.stop()
	reg, err := registry.Open(s.path)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := reg.SetSecretKeys(registry.SecretKeys{
		Primary: primary, Legacy: [][]byte{registry.LegacyTokenKey(adminToken)},
	}); err != nil {
		s.t.Fatal(err)
	}
	if _, err := reg.ReencryptSecrets(); err != nil {
		s.t.Fatal(err)
	}
	eng := engine.New(reg, s.d, "postgres:17", engine.WithCredentialRotation())
	s.reg = reg
	s.ts = httptest.NewServer(New(eng, reg, adminToken, nil, nil, 0).Handler())
	s.t.Cleanup(s.stop)
}

func (s *restartable) stop() {
	if s.ts != nil {
		s.ts.Close()
		s.ts = nil
	}
	if s.reg != nil {
		s.reg.Close()
		s.reg = nil
	}
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func (s *restartable) createBranch(token, name string) Branch {
	s.t.Helper()
	code, body := do(s.t, s.ts, token, "POST", "/v1/branches", CreateBranchRequest{Name: name, Source: "main"})
	if code != http.StatusCreated {
		s.t.Fatalf("create %s: %d %s", name, code, body)
	}
	b := mustUnmarshal[Branch](s.t, body)
	if b.Password == "" {
		s.t.Fatalf("create %s returned no rotated password", name)
	}
	return b
}

func (s *restartable) addSource(token string) {
	s.t.Helper()
	code, body := do(s.t, s.ts, token, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Port: 5432, PGVersion: "17", Password: "secret",
	})
	if code != http.StatusCreated {
		s.t.Fatalf("create source: %d %s", code, body)
	}
}

// SECRETS-01/02: rotating PGOVERLAY_TOKEN (the documented response to a leaked
// token) changes nothing for stored passwords, because the at-rest key is
// independent of it; the old token stops working.
func TestAdminTokenRotationKeepsBranchPasswords(t *testing.T) {
	s := newRestartable(t)
	key := randomKey(t)
	s.start(tokenA, key)
	s.addSource(tokenA)
	created := s.createBranch(tokenA, "rot1")

	s.start(tokenB, key) // restart with a rotated admin token

	if code, _ := do(t, s.ts, tokenA, "GET", "/v1/branches", nil); code != http.StatusUnauthorized {
		t.Fatalf("old token: %d, want 401", code)
	}
	code, body := do(t, s.ts, tokenB, "GET", "/v1/branches/rot1", nil)
	if code != http.StatusOK {
		t.Fatalf("get after token rotation: %d %s", code, body)
	}
	if got := mustUnmarshal[Branch](t, body); got.Password != created.Password || got.PasswordUnavailable {
		t.Fatalf("after token rotation: password=%q unavailable=%v, want the original password", got.Password, got.PasswordUnavailable)
	}
	if code, body := do(t, s.ts, tokenB, "GET", "/v1/reconcile/plan", nil); code != http.StatusOK {
		t.Fatalf("reconcile plan: %d %s", code, body)
	}
}

// Upgrade from a pre-v1 registry, whose passwords were encrypted under
// sha256(PGOVERLAY_TOKEN): the first start with the dedicated key reads them
// through the legacy fallback and re-encrypts them, so a later token rotation
// keeps them readable.
func TestLegacyTokenEncryptedRowsSurviveUpgradeThenRotation(t *testing.T) {
	s := newRestartable(t)
	key := randomKey(t)
	s.start(tokenA, key)
	s.addSource(tokenA)
	s.createBranch(tokenA, "old1")
	s.stop()

	// rewrite the row the way the pre-v1 build stored it
	const legacyPW = "0123456789abcdef0123456789abcdef"
	db, err := sql.Open("sqlite", s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE branches SET password=? WHERE name='old1'`, sealV1(t, tokenA, legacyPW)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s.start(tokenA, key) // upgraded branchd, same token: legacy read + re-encrypt
	s.start(tokenB, key) // then the token is rotated
	code, body := do(t, s.ts, tokenB, "GET", "/v1/branches/old1", nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	if got := mustUnmarshal[Branch](t, body); got.Password != legacyPW {
		t.Fatalf("legacy row after upgrade + token rotation: %+v", got)
	}
}

// SECRETS-01/02: when the at-rest key itself is lost or replaced, branches
// with undecryptable passwords stay listable and reconcilable, report
// password_unavailable, and can be reset (re-minting the password) or
// destroyed. Nothing returns 500.
func TestLostAtRestKeyDegradesToPasswordUnavailable(t *testing.T) {
	s := newRestartable(t)
	s.start(tokenA, randomKey(t))
	s.addSource(tokenA)
	s.createBranch(tokenA, "rot1")
	s.createBranch(tokenA, "rot2")

	s.start(tokenA, randomKey(t)) // key file lost: a new key is generated

	code, body := do(t, s.ts, tokenA, "GET", "/v1/branches", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	list := mustUnmarshal[[]Branch](t, body)
	if len(list) != 2 {
		t.Fatalf("list returned %d branches, want 2", len(list))
	}
	for _, b := range list {
		if !b.PasswordUnavailable || b.Password != "" {
			t.Fatalf("list entry %+v, want password_unavailable and no password", b)
		}
	}
	if !strings.Contains(string(body), `"password_unavailable":true`) {
		t.Fatalf("wire body lacks password_unavailable: %s", body)
	}
	if code, body := do(t, s.ts, tokenA, "GET", "/v1/branches/rot1", nil); code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	if code, body := do(t, s.ts, tokenA, "GET", "/v1/reconcile/plan", nil); code != http.StatusOK {
		t.Fatalf("reconcile plan: %d %s", code, body)
	}

	// the documented recovery: reset re-mints a password under the new key
	code, body = do(t, s.ts, tokenA, "POST", "/v1/branches/rot1/reset", nil)
	if code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, body)
	}
	if b := mustUnmarshal[Branch](t, body); b.PasswordUnavailable || b.Password == "" {
		t.Fatalf("after reset: %+v, want a fresh readable password", b)
	}
	if code, body := do(t, s.ts, tokenA, "DELETE", "/v1/branches/rot2", nil); code != http.StatusNoContent {
		t.Fatalf("destroy: %d %s", code, body)
	}
}

// sealV1 encrypts like the pre-v1 build: enc:v1: + base64(nonce || AES-GCM)
// under sha256(token).
func sealV1(t *testing.T, token, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(registry.LegacyTokenKey(token))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return "enc:v1:" + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plaintext), nil))
}
