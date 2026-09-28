package registry

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Stored password formats. A value without the encPrefix is plaintext: a legacy
// row written before at-rest encryption, or one written while no key was
// configured. Bumping the scheme means a new version tag, so older rows stay
// decodable by their own branch of decrypt.
//
//	enc:v1:<base64(nonce||ct)>        AES-256-GCM, key = sha256(PGOVERLAY_TOKEN).
//	                                  Read-only: legacy rows from before the
//	                                  dedicated at-rest key existed.
//	enc:v2:<kid>:<base64(nonce||ct)>  AES-256-GCM under the dedicated at-rest key
//	                                  whose key id is kid (see keyID).
const (
	encPrefix   = "enc:"
	encPrefixV1 = "enc:v1:"
	encPrefixV2 = "enc:v2:"
)

// ErrSecretUnavailable reports a stored branch password that none of the
// configured keys can decrypt: the at-rest key changed, or the row predates the
// dedicated key and was encrypted under a PGOVERLAY_TOKEN that is no longer set.
// Read paths do not fail on it; they return the branch with
// Branch.PasswordUnavailable set, and resetting the branch mints a new password.
var ErrSecretUnavailable = errors.New("branch password cannot be decrypted with the configured secret key")

// SecretKeys configures at-rest encryption of branch passwords (see
// Registry.SetSecretKeys).
type SecretKeys struct {
	// Primary is the dedicated 32-byte at-rest key. It encrypts every new
	// password (enc:v2:) and decrypts rows carrying its key id. nil disables
	// encryption: new passwords are stored as plaintext.
	Primary []byte
	// Legacy holds decrypt-only keys for enc:v1: rows, which were encrypted
	// under sha256(PGOVERLAY_TOKEN) (see LegacyTokenKey). They let an upgraded
	// registry read its old rows so ReencryptSecrets can move them under
	// Primary.
	Legacy [][]byte
}

// LegacyTokenKey returns the pre-v1 at-rest key for a PGOVERLAY_TOKEN:
// sha256(token). It only decrypts legacy enc:v1: rows; nothing is encrypted
// under it any more. Returns nil for an empty token.
func LegacyTokenKey(token string) []byte {
	if token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// KeyID returns the identifier stored in front of every enc:v2: value
// encrypted under key: the first 4 bytes, hex-encoded, of a domain-separated
// SHA-256 of the key. It names the key without revealing it, so a registry
// can tell which key a row needs. Returns "" for an invalid key.
func KeyID(key []byte) string {
	if len(key) != 32 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("pgoverlay/at-rest-key-id/v1\x00"))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil)[:4])
}

// secretBox encrypts and decrypts branch passwords at rest (AES-256-GCM with
// a random 12-byte nonce). It is optional on the Registry: a nil *secretBox
// means no key is configured, so passwords are stored and read as plaintext
// and any encrypted row reads as unavailable.
type secretBox struct {
	primaryID string                 // key id of the encrypting key; "" = encryption off
	byID      map[string]cipher.AEAD // enc:v2: keys by key id (primary included)
	legacy    []cipher.AEAD          // enc:v1: keys, tried in order
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("registry secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// newSecretBox builds a secretBox. No keys at all yields a nil box
// (encryption disabled); a wrong-length key is a configuration error.
func newSecretBox(k SecretKeys) (*secretBox, error) {
	if len(k.Primary) == 0 && len(k.Legacy) == 0 {
		return nil, nil
	}
	b := &secretBox{byID: map[string]cipher.AEAD{}}
	if len(k.Primary) > 0 {
		aead, err := newAEAD(k.Primary)
		if err != nil {
			return nil, err
		}
		b.primaryID = KeyID(k.Primary)
		b.byID[b.primaryID] = aead
	}
	for _, key := range k.Legacy {
		if len(key) == 0 {
			continue
		}
		aead, err := newAEAD(key)
		if err != nil {
			return nil, fmt.Errorf("legacy key: %w", err)
		}
		b.legacy = append(b.legacy, aead)
	}
	return b, nil
}

// encrypt returns the at-rest form of a plaintext password:
// enc:v2:<kid>:base64(nonce || ciphertext). An empty plaintext stays empty
// (inherit mode stores "", nothing to protect). With no primary key the
// plaintext is returned unchanged (encryption disabled).
func (b *secretBox) encrypt(plaintext string) (string, error) {
	if b == nil || b.primaryID == "" || plaintext == "" {
		return plaintext, nil
	}
	aead := b.byID[b.primaryID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefixV2 + b.primaryID + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

// decrypt reverses encrypt. A value without the enc: prefix is legacy
// plaintext and is returned as-is. An encrypted value that no configured key
// opens returns an error wrapping ErrSecretUnavailable, never the ciphertext.
// The error text never contains the stored value.
func (b *secretBox) decrypt(stored string) (string, error) {
	switch {
	case !strings.HasPrefix(stored, encPrefix):
		return stored, nil // legacy plaintext / inherit-mode empty
	case strings.HasPrefix(stored, encPrefixV2):
		kid, body, ok := strings.Cut(strings.TrimPrefix(stored, encPrefixV2), ":")
		if !ok {
			return "", fmt.Errorf("%w: malformed enc:v2 value", ErrSecretUnavailable)
		}
		var aead cipher.AEAD
		if b != nil {
			aead = b.byID[kid]
		}
		if aead == nil {
			return "", fmt.Errorf("%w: it was encrypted under at-rest key %s, which is not configured", ErrSecretUnavailable, kid)
		}
		pt, err := openSealed(aead, body)
		if err != nil {
			return "", fmt.Errorf("%w: at-rest key %s: %v", ErrSecretUnavailable, kid, err)
		}
		return pt, nil
	case strings.HasPrefix(stored, encPrefixV1):
		body := strings.TrimPrefix(stored, encPrefixV1)
		if b != nil {
			for _, aead := range b.legacy {
				if pt, err := openSealed(aead, body); err == nil {
					return pt, nil
				}
			}
		}
		return "", fmt.Errorf("%w: it predates the dedicated at-rest key and was encrypted under a PGOVERLAY_TOKEN that is not the current one", ErrSecretUnavailable)
	default:
		return "", fmt.Errorf("%w: unknown encryption scheme", ErrSecretUnavailable)
	}
}

// openSealed decodes base64(nonce || ciphertext) and authenticates/decrypts it.
func openSealed(aead cipher.AEAD, body string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	ns := aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// isCurrent reports whether a stored value is already in its final form: empty,
// or encrypted under the primary key. Anything else (plaintext, enc:v1:, or
// enc:v2: under another key) is a candidate for ReencryptSecrets. With no
// primary key every value is current: there is nothing to move it to.
func (b *secretBox) isCurrent(stored string) bool {
	if stored == "" || b == nil || b.primaryID == "" {
		return true
	}
	return strings.HasPrefix(stored, encPrefixV2+b.primaryID+":")
}
