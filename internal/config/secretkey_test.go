package config

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	t.Setenv("PGOVERLAY_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv(SecretKeyEnv, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseSecretKey(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab}, 32)
	for _, s := range []string{
		hex.EncodeToString(raw),
		hex.EncodeToString(raw) + "\n",
		base64.StdEncoding.EncodeToString(raw),
		"  " + base64.RawURLEncoding.EncodeToString(raw) + "\n",
	} {
		got, err := ParseSecretKey(s)
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("ParseSecretKey(%q) = %x, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "short", hex.EncodeToString(raw[:16]), strings.Repeat("z", 64)} {
		if _, err := ParseSecretKey(bad); err == nil {
			t.Errorf("ParseSecretKey(%q) accepted", bad)
		}
	}
}

// First start generates <state dir>/secret.key 0600; later starts (and the
// CLI, which never generates) read the same key.
func TestLoadSecretKeyGeneratesOnceIntoStateDir(t *testing.T) {
	c := testConfig(t)
	if key, origin, err := c.LoadSecretKey("", false); err != nil || key != nil || origin != "" {
		t.Fatalf("no key and no generate: key=%x origin=%q err=%v, want nothing", key, origin, err)
	}
	if _, err := os.Stat(c.SecretKeyFile()); !os.IsNotExist(err) {
		t.Fatalf("generate=false created a key file: %v", err)
	}

	key, origin, err := c.LoadSecretKey("", true)
	if err != nil || len(key) != 32 || origin != c.SecretKeyFile() {
		t.Fatalf("generate: key=%x origin=%q err=%v", key, origin, err)
	}
	fi, err := os.Stat(c.SecretKeyFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode %o, want 600", perm)
	}
	again, _, err := c.LoadSecretKey("", true)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("second load = %x, %v; want the generated key", again, err)
	}
	cli, _, err := c.LoadSecretKey("", false)
	if err != nil || !bytes.Equal(cli, key) {
		t.Fatalf("read-only load = %x, %v; want the generated key", cli, err)
	}
	entries, _ := os.ReadDir(c.Home)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".secret.key-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// Replicas starting together on one state dir must agree on one key.
func TestLoadSecretKeyConcurrentGenerationAgrees(t *testing.T) {
	c := testConfig(t)
	const n = 8
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			keys[i], _, errs[i] = c.LoadSecretKey("", true)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range keys {
		if errs[i] != nil || !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("starter %d: key=%x err=%v, want all equal to %x", i, keys[i], errs[i], keys[0])
		}
	}
}

func TestLoadSecretKeyPrecedence(t *testing.T) {
	c := testConfig(t)
	fileKey := bytes.Repeat([]byte{1}, 32)
	envKey := bytes.Repeat([]byte{2}, 32)
	f := filepath.Join(t.TempDir(), "mounted.key")
	if err := os.WriteFile(f, []byte(hex.EncodeToString(fileKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	key, origin, err := c.LoadSecretKey(f, true)
	if err != nil || !bytes.Equal(key, fileKey) || origin != f {
		t.Fatalf("explicit file: key=%x origin=%q err=%v", key, origin, err)
	}
	if _, err := os.Stat(c.SecretKeyFile()); !os.IsNotExist(err) {
		t.Fatal("an explicit key file must not generate the state-dir key")
	}

	t.Setenv(SecretKeyEnv, base64.StdEncoding.EncodeToString(envKey))
	key, origin, err = c.LoadSecretKey(f, true)
	if err != nil || !bytes.Equal(key, envKey) || origin != "$"+SecretKeyEnv {
		t.Fatalf("env: key=%x origin=%q err=%v", key, origin, err)
	}

	t.Setenv(SecretKeyEnv, "not-a-key")
	if _, _, err := c.LoadSecretKey("", true); err == nil || !strings.Contains(err.Error(), SecretKeyEnv) {
		t.Fatalf("malformed env key err=%v", err)
	}
	t.Setenv(SecretKeyEnv, "")
	if _, _, err := c.LoadSecretKey(filepath.Join(t.TempDir(), "missing.key"), true); err == nil {
		t.Fatal("a missing explicit key file must be an error, not silently generated")
	}
}
