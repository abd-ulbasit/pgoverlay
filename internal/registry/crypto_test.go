package registry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testKey returns a fresh random 32-byte at-rest key.
func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// sealLegacyV1 produces a value exactly as pre-v1 branchd stored it:
// enc:v1: + base64(nonce || AES-256-GCM ciphertext) under sha256(token).
func sealLegacyV1(t *testing.T, token, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(LegacyTokenKey(token))
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
	return encPrefixV1 + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, []byte(plaintext), nil))
}

// setRawPassword writes the password column directly, bypassing encryption,
// to stage rows as an older build (or another key) left them.
func setRawPassword(t *testing.T, r *Registry, id, stored string) {
	t.Helper()
	if _, err := r.db.Exec(`UPDATE branches SET password=? WHERE id=?`, stored, id); err != nil {
		t.Fatal(err)
	}
}

func TestKeyID(t *testing.T) {
	a, b := testKey(t), testKey(t)
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(KeyID(a)) {
		t.Fatalf("KeyID=%q want 8 hex chars", KeyID(a))
	}
	if KeyID(a) != KeyID(append([]byte(nil), a...)) {
		t.Fatal("KeyID is not deterministic")
	}
	if KeyID(a) == KeyID(b) {
		t.Fatal("two random keys share a key id")
	}
	if KeyID([]byte("short")) != "" {
		t.Fatal("KeyID of an invalid key should be empty")
	}
}

// SECRETS-01/02: a stored password the configured key cannot decrypt must not
// fail any read. Every multi-row reader (list, stuck, expired) still returns
// every row, single-row reads return the branch flagged PasswordUnavailable,
// and the reset path (SetBranchPassword) and destroy path (transitions) work,
// after which the branch reads normally again.
func TestUndecryptablePasswordDoesNotFailReads(t *testing.T) {
	r := openTest(t)
	if err := r.SetSecretKey(testKey(t)); err != nil {
		t.Fatal(err)
	}
	rot := makeBranch(t, r, "rot")
	if err := r.SetBranchPassword(rot.ID, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	plain := makeBranch(t, r, "plain") // inherit mode, no password
	if err := r.MarkBranchReady(plain.ID, "cid-plain", "127.0.0.1", 5432); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(rot.ID, "cid-rot", "127.0.0.1", 5433); err != nil {
		t.Fatal(err)
	}

	// the at-rest key changes (lost key file, new key): rot is now unreadable
	if err := r.SetSecretKey(testKey(t)); err != nil {
		t.Fatal(err)
	}

	got, err := r.GetBranchByName("rot")
	if err != nil {
		t.Fatalf("GetBranchByName failed on an undecryptable row: %v", err)
	}
	if !got.PasswordUnavailable || got.Password != "" {
		t.Fatalf("got Password=%q PasswordUnavailable=%v; want empty + flagged", got.Password, got.PasswordUnavailable)
	}
	if p, err := r.GetBranchByName("plain"); err != nil || p.PasswordUnavailable {
		t.Fatalf("inherit-mode branch: %+v err=%v", p, err)
	}
	live, err := r.ListLiveBranches()
	if err != nil || len(live) != 2 {
		t.Fatalf("ListLiveBranches = %d rows, err=%v; want both rows", len(live), err)
	}
	if _, err := r.LayerChain(rot.ID); err != nil {
		t.Fatalf("LayerChain: %v", err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if _, err := r.ListStuckBranches(future); err != nil {
		t.Fatalf("ListStuckBranches: %v", err)
	}
	if _, err := r.ListExpiredBranches(future); err != nil {
		t.Fatalf("ListExpiredBranches: %v", err)
	}

	// reset re-mints: the new password is encrypted under the current key
	if err := r.TransitionBranch(rot.ID, BranchResetting, "reset requested"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBranchPassword(rot.ID, "fedcba9876543210"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(rot.ID, "cid-rot2", "127.0.0.1", 5434); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetBranchByName("rot"); err != nil || got.PasswordUnavailable || got.Password != "fedcba9876543210" {
		t.Fatalf("after reset: %+v err=%v", got, err)
	}

	// destroy of an unreadable branch works too
	setRawPassword(t, r, plain.ID, sealLegacyV1(t, "some-old-token-value", "x"))
	for _, to := range []BranchState{BranchDestroying, BranchDestroyed} {
		if err := r.TransitionBranch(plain.ID, to, ""); err != nil {
			t.Fatalf("-> %s: %v", to, err)
		}
	}
}

// Decrypt failures never echo the stored value (ciphertext stays out of
// errors and logs) and always match ErrSecretUnavailable.
func TestDecryptErrorsAreOpaqueAndTyped(t *testing.T) {
	box, err := newSecretBox(SecretKeys{Primary: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := newSecretBox(SecretKeys{Primary: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := other.encrypt("pw")
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []string{
		v2,                                      // another key
		sealLegacyV1(t, "legacy-token-1", "pw"), // legacy, token not configured
		"enc:v2:nokeyid",                        // malformed
		"enc:v9:whatever",                       // unknown scheme
	} {
		_, err := box.decrypt(stored)
		if !errors.Is(err, ErrSecretUnavailable) {
			t.Errorf("decrypt(%.12q...) err=%v want ErrSecretUnavailable", stored, err)
			continue
		}
		if body := stored[strings.LastIndex(stored, ":")+1:]; len(body) > 4 && strings.Contains(err.Error(), body) {
			t.Errorf("error %q echoes the stored value", err)
		}
	}
	// a nil box (no key configured) reads plaintext and flags ciphertext
	var none *secretBox
	if pt, err := none.decrypt("plain"); err != nil || pt != "plain" {
		t.Fatalf("nil box plaintext: %q %v", pt, err)
	}
	if _, err := none.decrypt(v2); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("nil box ciphertext err=%v", err)
	}
}

// Upgrade path: rows a pre-v1 branchd encrypted under sha256(PGOVERLAY_TOKEN)
// read through the legacy fallback, ReencryptSecrets moves them under the
// dedicated key, and from then on the token can rotate freely.
func TestLegacyTokenRowsReencryptAndSurviveTokenRotation(t *testing.T) {
	r := openTest(t)
	b := makeBranch(t, r, "pr-1")
	const oldToken, newToken = "old-admin-token-0001", "new-admin-token-0002"
	setRawPassword(t, r, b.ID, sealLegacyV1(t, oldToken, "legacypassword01"))

	key := testKey(t)
	if err := r.SetSecretKeys(SecretKeys{Primary: key, Legacy: [][]byte{LegacyTokenKey(oldToken)}}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetBranchByName("pr-1"); err != nil || got.Password != "legacypassword01" {
		t.Fatalf("legacy read: %+v err=%v", got, err)
	}
	rep, err := r.ReencryptSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reencrypted != 1 || len(rep.Unavailable) != 0 {
		t.Fatalf("report %+v want 1 re-encrypted", rep)
	}
	if raw := rawBranchPassword(t, r, b.ID); !strings.HasPrefix(raw, encPrefixV2+KeyID(key)+":") {
		t.Fatalf("raw after re-encrypt = %q, want enc:v2 under the dedicated key", raw)
	}

	// PGOVERLAY_TOKEN rotated (restart with the new token): nothing breaks
	if err := r.SetSecretKeys(SecretKeys{Primary: key, Legacy: [][]byte{LegacyTokenKey(newToken)}}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetBranchByName("pr-1"); err != nil || got.PasswordUnavailable || got.Password != "legacypassword01" {
		t.Fatalf("after token rotation: %+v err=%v", got, err)
	}
	// a second sweep has nothing left to do
	if rep, err := r.ReencryptSecrets(); err != nil || rep.Reencrypted != 0 {
		t.Fatalf("second sweep %+v err=%v", rep, err)
	}
}

// Rotating the at-rest key itself: rows under a previous (decrypt-only) key
// stay readable and the sweep moves them under the new primary, after which
// the previous key is no longer needed.
func TestPreviousKeyRowsMoveToPrimary(t *testing.T) {
	r := openTest(t)
	oldKey, newKey := testKey(t), testKey(t)
	if err := r.SetSecretKey(oldKey); err != nil {
		t.Fatal(err)
	}
	b := makeBranch(t, r, "pr-1")
	if err := r.SetBranchPassword(b.ID, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}

	if err := r.SetSecretKeys(SecretKeys{Primary: newKey, Previous: [][]byte{oldKey}}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetBranchByName("pr-1"); err != nil || got.Password != "0123456789abcdef" {
		t.Fatalf("read under a previous key: %+v err=%v", got, err)
	}
	rep, err := r.ReencryptSecrets()
	if err != nil || rep.Reencrypted != 1 {
		t.Fatalf("sweep %+v err=%v, want 1 re-encrypted", rep, err)
	}
	if raw := rawBranchPassword(t, r, b.ID); !strings.HasPrefix(raw, encPrefixV2+KeyID(newKey)+":") {
		t.Fatalf("raw %q not under the new key", raw)
	}
	if err := r.SetSecretKey(newKey); err != nil { // previous key dropped
		t.Fatal(err)
	}
	if got, err := r.GetBranchByName("pr-1"); err != nil || got.PasswordUnavailable || got.Password != "0123456789abcdef" {
		t.Fatalf("after dropping the previous key: %+v err=%v", got, err)
	}
	if err := r.SetSecretKeys(SecretKeys{Primary: newKey, Previous: [][]byte{[]byte("short")}}); err == nil {
		t.Fatal("a malformed previous key was accepted")
	}
}

// SECRETS-07: legacy plaintext rows are encrypted by the sweep; rows no key
// can open are reported and left untouched; destroyed tombstones and inherit
// rows are ignored; without a primary key the sweep is a no-op.
func TestReencryptSecrets(t *testing.T) {
	r := openTest(t)
	plain := makeBranch(t, r, "plain")
	if err := r.SetBranchPassword(plain.ID, "plaintextpw00001"); err != nil { // no key yet
		t.Fatal(err)
	}
	lost := makeBranch(t, r, "lost")
	lostStored := sealLegacyV1(t, "a-token-nobody-has-now", "gone")
	setRawPassword(t, r, lost.ID, lostStored)
	makeBranch(t, r, "inherit")

	if rep, err := r.ReencryptSecrets(); err != nil || rep.Reencrypted != 0 || len(rep.Unavailable) != 0 {
		t.Fatalf("no-key sweep %+v err=%v; want no-op", rep, err)
	}
	if raw := rawBranchPassword(t, r, plain.ID); raw != "plaintextpw00001" {
		t.Fatalf("no-key sweep rewrote a row: %q", raw)
	}

	if err := r.SetSecretKey(testKey(t)); err != nil {
		t.Fatal(err)
	}
	rep, err := r.ReencryptSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reencrypted != 1 || len(rep.Unavailable) != 1 || rep.Unavailable[0] != "lost" {
		t.Fatalf("report %+v want 1 re-encrypted and [lost] unavailable", rep)
	}
	if raw := rawBranchPassword(t, r, plain.ID); strings.Contains(raw, "plaintextpw00001") || !strings.HasPrefix(raw, encPrefixV2) {
		t.Fatalf("plaintext row not encrypted: %q", raw)
	}
	if got, _ := r.GetBranchByName("plain"); got.Password != "plaintextpw00001" {
		t.Fatalf("re-encrypted row reads %q", got.Password)
	}
	if raw := rawBranchPassword(t, r, lost.ID); raw != lostStored {
		t.Fatalf("unreadable row was rewritten: %q", raw)
	}
}

// SECRETS-07: a branch that reaches 'destroyed' keeps no password, whatever
// path destroyed it (enforced by the v12 trigger).
func TestDestroyedBranchForgetsPassword(t *testing.T) {
	r := openTest(t)
	if err := r.SetSecretKey(testKey(t)); err != nil {
		t.Fatal(err)
	}
	b := seedReadyBranch(t, r, "pr-gone")
	if err := r.SetBranchPassword(b.ID, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranch(b.ID, BranchDestroying, ""); err != nil {
		t.Fatal(err)
	}
	if raw := rawBranchPassword(t, r, b.ID); raw == "" {
		t.Fatal("password cleared before the branch was destroyed")
	}
	if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	if raw := rawBranchPassword(t, r, b.ID); raw != "" {
		t.Fatalf("destroyed tombstone still carries a password: %q", raw)
	}
}
