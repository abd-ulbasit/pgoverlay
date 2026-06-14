package registry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

// encPrefix tags a value stored by encryptSecret as ciphertext (vs a
// legacy/plaintext value with no prefix). The v1 scheme is AES-256-GCM with a
// random 12-byte nonce prepended to the ciphertext and the whole thing
// base64-std-encoded after the prefix. Bumping the scheme (e.g. a new KDF or
// AEAD) means a new prefix (enc:v2:) so old rows stay decodable by their own
// version branch.
const encPrefix = "enc:v1:"

// secretBox encrypts/decrypts branch passwords at rest with a key derived from
// PGBRANCH_TOKEN (key = sha256(token); see DeriveSecretKey). It is OPTIONAL on
// the Registry: a nil *secretBox means no key was configured and the registry
// stores/reads passwords as plaintext (back-compat for inherit-mode setups and
// tests). A non-nil box encrypts on write and decrypts the enc:-prefixed rows
// on read, while still passing legacy plaintext rows through untouched.
//
// Trade-off: the key is derived from PGBRANCH_TOKEN, so rotating the token
// makes every existing encrypted branch password UNRECOVERABLE (decrypt fails
// with the wrong key). That is acceptable for pgbranch's ephemeral branches —
// re-run credential rotation (reset the branch) after a token change to mint a
// password the new key can decrypt. Documented in docs/usage.md.
type secretBox struct {
	aead cipher.AEAD
}

// DeriveSecretKey returns the 32-byte AES-256 key for a PGBRANCH_TOKEN:
// key = sha256(token). Returns nil for an empty token so callers can pass it
// straight into SetSecretKey (a nil key disables at-rest encryption).
func DeriveSecretKey(token string) []byte {
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// newSecretBox builds a secretBox from a 32-byte key. A nil/empty key yields a
// nil box (encryption disabled); a wrong-length key is a programming error and
// is rejected.
func newSecretBox(key []byte) (*secretBox, error) {
	if len(key) == 0 {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("registry secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead: aead}, nil
}

// encrypt returns the at-rest form of a plaintext password: the encPrefix
// followed by base64(nonce || ciphertext). An empty plaintext stays empty
// (inherit mode stores "" — nothing to protect). A nil box returns the
// plaintext unchanged (encryption disabled).
func (b *secretBox) encrypt(plaintext string) (string, error) {
	if b == nil || plaintext == "" {
		return plaintext, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

// decrypt reverses encrypt. A value WITHOUT the encPrefix is treated as legacy
// plaintext and returned as-is (back-compat: rows written before at-rest
// encryption, and rows written while no key was configured). A nil box returns
// any value as-is — but a nil box must never be asked to read an enc:-prefixed
// value, since it can't; that case errors so a misconfigured key surfaces
// loudly instead of leaking ciphertext to a client.
func (b *secretBox) decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil // legacy plaintext / inherit-mode empty
	}
	if b == nil {
		return "", fmt.Errorf("registry: encrypted branch password found but no secret key configured (PGBRANCH_TOKEN missing?)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("registry: decode encrypted password: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("registry: encrypted password too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := b.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// Almost always a wrong key: PGBRANCH_TOKEN was rotated after this row
		// was encrypted. Recoverable only by re-rotating the branch's creds.
		return "", fmt.Errorf("registry: decrypt branch password (PGBRANCH_TOKEN rotated since it was stored? re-run credential rotation): %w", err)
	}
	return string(pt), nil
}

// decryptColumn is the read-path helper used by scanBranch: it turns a stored
// password column value into the plaintext the API returns. nil box (no key)
// passes plaintext through.
func decryptColumn(b *secretBox, stored string) (string, error) {
	return b.decrypt(stored)
}
