package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables for the at-rest key that encrypts rotated branch
// passwords in the registry. The key is independent of PGOVERLAY_TOKEN, so the
// admin token can be rotated without touching stored passwords.
const (
	// SecretKeyEnv holds the key itself: 32 random bytes, hex- or
	// base64-encoded (e.g. `openssl rand -hex 32`).
	SecretKeyEnv = "PGOVERLAY_SECRET_KEY"
	// SecretKeyFileEnv names a file holding the key in the same encoding
	// (the default for branchd's --secret-key-file).
	SecretKeyFileEnv = "PGOVERLAY_SECRET_KEY_FILE"
	// SecretKeyFileName is the key file branchd generates in the state
	// directory when no key is configured.
	SecretKeyFileName = "secret.key"
	// SecretKeyPreviousEnv holds retired at-rest keys, comma-separated, in
	// the same encoding. They only decrypt, so rotating the at-rest key is:
	// set the new key, put the old one here for one start (branchd
	// re-encrypts every row under the new key), then drop it.
	SecretKeyPreviousEnv = "PGOVERLAY_SECRET_KEY_PREVIOUS"
)

// SecretKeyFile is the default at-rest key file: <state dir>/secret.key.
func (c *Config) SecretKeyFile() string { return filepath.Join(c.Home, SecretKeyFileName) }

// LoadSecretKey resolves the 32-byte at-rest key and reports where it came
// from. Precedence:
//
//  1. $PGOVERLAY_SECRET_KEY (the key itself);
//  2. keyFile, when non-empty (an explicit --secret-key-file or
//     $PGOVERLAY_SECRET_KEY_FILE); it must exist;
//  3. <state dir>/secret.key. When it is absent and generate is true, a new
//     random key is written there with mode 0600 (branchd does this on first
//     start); when generate is false the result is (nil, "", nil).
//
// Generation is atomic: concurrent first starts (HA replicas sharing one state
// dir) all end up with the key the first writer published.
func (c *Config) LoadSecretKey(keyFile string, generate bool) (key []byte, origin string, err error) {
	if v := os.Getenv(SecretKeyEnv); v != "" {
		key, err := ParseSecretKey(v)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", SecretKeyEnv, err)
		}
		return key, "$" + SecretKeyEnv, nil
	}
	if keyFile != "" {
		key, err := readKeyFile(keyFile)
		if err != nil {
			return nil, "", err
		}
		return key, keyFile, nil
	}
	path := c.SecretKeyFile()
	key, err = readKeyFile(path)
	if err == nil {
		return key, path, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	if !generate {
		return nil, "", nil
	}
	key, err = generateKeyFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("generate at-rest key %s: %w", path, err)
	}
	return key, path, nil
}

// PreviousSecretKeys returns the decrypt-only at-rest keys to accept next to
// primary: every key in $PGOVERLAY_SECRET_KEY_PREVIOUS, plus
// <state dir>/secret.key when it exists and differs from primary (the key an
// install used before the operator moved to $PGOVERLAY_SECRET_KEY or
// --secret-key-file). A malformed previous key is an error; a missing or
// unreadable state-dir file is not.
func (c *Config) PreviousSecretKeys(primary []byte) ([][]byte, error) {
	var out [][]byte
	for _, v := range strings.Split(os.Getenv(SecretKeyPreviousEnv), ",") {
		if strings.TrimSpace(v) == "" {
			continue
		}
		key, err := ParseSecretKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", SecretKeyPreviousEnv, err)
		}
		out = append(out, key)
	}
	if key, err := readKeyFile(c.SecretKeyFile()); err == nil && !bytes.Equal(key, primary) {
		out = append(out, key)
	}
	return out, nil
}

// ParseSecretKey decodes an at-rest key: 32 bytes as 64 hex characters or as
// standard base64. Surrounding whitespace (a trailing newline in a file) is
// ignored.
func ParseSecretKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("want 32 random bytes, hex- or base64-encoded (generate one with: openssl rand -hex 32)")
}

func readKeyFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read at-rest key: %w", err)
	}
	key, err := ParseSecretKey(string(raw))
	if err != nil {
		return nil, fmt.Errorf("at-rest key %s: %w", path, err)
	}
	return key, nil
}

// generateKeyFile writes a fresh random key to path (0600) without ever
// exposing a partial file: the key goes to a temp file in the same directory,
// which is then hard-linked into place. Link fails if path already exists, in
// which case the winner's key is read back instead.
func generateKeyFile(path string) ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".secret.key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	err = os.Link(tmp.Name(), path)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		// No hard links on this filesystem: fall back to an exclusive create,
		// which still never overwrites a key another starter published.
		err = writeExclusive(path, hex.EncodeToString(key)+"\n")
	}
	if errors.Is(err, fs.ErrExist) {
		return readKeyFile(path) // lost the race: use the published key
	}
	if err != nil {
		return nil, err
	}
	return key, nil
}

func writeExclusive(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
