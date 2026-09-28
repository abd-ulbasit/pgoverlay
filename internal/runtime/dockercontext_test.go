package runtime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerConfig creates an isolated docker config dir (DOCKER_CONFIG) with no
// DOCKER_HOST / DOCKER_CONTEXT / TLS env leaking in from the developer's shell.
func dockerConfig(t *testing.T, current string) string {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{
		"DOCKER_CONFIG": dir, "DOCKER_HOST": "", "DOCKER_CONTEXT": "",
		"DOCKER_CERT_PATH": "", "DOCKER_TLS_VERIFY": "",
	} {
		t.Setenv(k, v)
	}
	if current != "" {
		writeJSONFile(t, filepath.Join(dir, "config.json"), map[string]string{"currentContext": current})
	}
	return dir
}

// addContext stores a context the way `docker context create` does.
func addContext(t *testing.T, dir, name, host string, skipTLS bool) string {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(sum[:])
	writeJSONFile(t, filepath.Join(dir, "contexts", "meta", id, "meta.json"), map[string]any{
		"Name":      name,
		"Endpoints": map[string]any{"docker": map[string]any{"Host": host, "SkipTLSVerify": skipTLS}},
	})
	return filepath.Join(dir, "contexts", "tls", id, "docker")
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCLIContextResolution(t *testing.T) {
	dir := dockerConfig(t, "colima")
	addContext(t, dir, "colima", "unix:///Users/me/.colima/default/docker.sock", false)
	addContext(t, dir, "other", "tcp://10.0.0.5:2375", false)

	if got := DockerHostFromCLIContext(); got != "unix:///Users/me/.colima/default/docker.sock" {
		t.Fatalf("config.json context: host = %q", got)
	}
	// DOCKER_CONTEXT wins over config.json, like the docker CLI
	t.Setenv("DOCKER_CONTEXT", "other")
	if got := DockerHostFromCLIContext(); got != "tcp://10.0.0.5:2375" {
		t.Fatalf("DOCKER_CONTEXT: host = %q", got)
	}
	t.Setenv("DOCKER_CONTEXT", "default")
	if got := DockerHostFromCLIContext(); got != "" {
		t.Fatalf("default context: host = %q, want the SDK default", got)
	}
	// an explicitly named context that does not exist is an error...
	t.Setenv("DOCKER_CONTEXT", "missing")
	if _, err := NewDockerDriver(); err == nil || !strings.Contains(err.Error(), `docker context "missing"`) {
		t.Fatalf("DOCKER_CONTEXT=missing: err = %v", err)
	}
	// ...a stale currentContext in config.json falls back to the default
	t.Setenv("DOCKER_CONTEXT", "")
	writeJSONFile(t, filepath.Join(dir, "config.json"), map[string]string{"currentContext": "gone"})
	if got := DockerHostFromCLIContext(); got != "" {
		t.Fatalf("stale currentContext: host = %q", got)
	}
	if _, err := NewDockerDriver(); err != nil {
		t.Fatalf("stale currentContext: %v", err)
	}
}

func TestNewDockerDriverUsesContextEndpoint(t *testing.T) {
	dir := dockerConfig(t, "colima")
	addContext(t, dir, "colima", "unix:///tmp/pgoverlay-test-docker.sock", false)
	d, err := NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	if got := d.cli.DaemonHost(); got != "unix:///tmp/pgoverlay-test-docker.sock" {
		t.Fatalf("daemon host = %q", got)
	}
}

// CLI-01 / CLI-11: an ssh:// endpoint used to become a plain HTTP request to
// port 80 of the ssh host.
func TestNewDockerDriverRejectsSSH(t *testing.T) {
	dir := dockerConfig(t, "thinkpad")
	addContext(t, dir, "thinkpad", "ssh://thinkpad", false)
	_, err := NewDockerDriver()
	if err == nil {
		t.Fatal("ssh:// context accepted")
	}
	for _, want := range []string{"ssh://thinkpad", `docker context "thinkpad"`, "cannot dial", "DOCKER_HOST", "--server"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}

	t.Setenv("DOCKER_HOST", "ssh://user@build-box")
	if _, err := NewDockerDriver(); err == nil || !strings.Contains(err.Error(), "from DOCKER_HOST") {
		t.Fatalf("DOCKER_HOST=ssh://: err = %v", err)
	}
	t.Setenv("DOCKER_HOST", "fd://")
	if _, err := NewDockerDriver(); err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("DOCKER_HOST=fd://: err = %v", err)
	}
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	if _, err := NewDockerDriver(); err != nil {
		t.Fatalf("DOCKER_HOST=tcp://: %v", err)
	}
}

// fakeTLSDaemon is an https server answering the Docker API ping, standing in
// for a daemon behind `dockerd --tlsverify`.
func fakeTLSDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		w.Header().Set("Ostype", "linux")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// A tcp:// context created with a CA: the SDK used to get only the host and
// spoke plain HTTP to the TLS daemon.
func TestNewDockerDriverUsesContextTLS(t *testing.T) {
	ts := fakeTLSDaemon(t)
	host := "tcp://" + strings.TrimPrefix(ts.URL, "https://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// the context stores the daemon's CA: verified TLS works
	dir := dockerConfig(t, "remote-tls")
	tlsDir := addContext(t, dir, "remote-tls", host, false)
	writePEM(t, filepath.Join(tlsDir, "ca.pem"), ts.Certificate().Raw)
	d, err := NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.cli.Ping(ctx); err != nil {
		t.Fatalf("ping over the context's TLS: %v", err)
	}

	// same endpoint without TLS material: plain HTTP to a TLS port fails
	dir = dockerConfig(t, "plain")
	addContext(t, dir, "plain", host, false)
	if d, err = NewDockerDriver(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cli.Ping(ctx); err == nil {
		t.Fatal("ping without TLS material succeeded; the TLS test above proves nothing")
	}

	// SkipTLSVerify with no material: TLS without verification
	dir = dockerConfig(t, "insecure")
	addContext(t, dir, "insecure", host, true)
	if d, err = NewDockerDriver(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.cli.Ping(ctx); err != nil {
		t.Fatalf("ping with SkipTLSVerify: %v", err)
	}
}

// Client certificates stored with the context are loaded; a malformed pair is
// reported instead of being ignored.
func TestContextTLSClientCertificate(t *testing.T) {
	dir := dockerConfig(t, "mtls")
	tlsDir := addContext(t, dir, "mtls", "tcp://docker.example:2376", false)
	writeCertAndKey(t, tlsDir)
	if _, err := NewDockerDriver(); err != nil {
		t.Fatalf("valid ca/cert/key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "key.pem"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDockerDriver(); err == nil || !strings.Contains(err.Error(), `docker context "mtls" TLS material`) {
		t.Fatalf("bad key.pem: err = %v", err)
	}
}

func writePEM(t *testing.T, path string, der []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCertAndKey writes a self-signed ca.pem plus a matching cert.pem and
// key.pem into dir.
func writeCertAndKey(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pgoverlay-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for name, data := range map[string][]byte{
		"ca.pem":   certPEM,
		"cert.pem": certPEM,
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
