package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestCertPair writes a self-signed PEM cert+key pair into dir and
// returns their paths.
func writeTestCertPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pgbranch-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestTLSConfigFromFlags(t *testing.T) {
	cert, key := writeTestCertPair(t, t.TempDir())

	// neither flag: TLS off, no error
	cfg, err := tlsConfigFromFlags("", "", "pg")
	if err != nil || cfg != nil {
		t.Fatalf("no flags: cfg=%v err=%v, want nil,nil", cfg, err)
	}

	// one without the other = startup error naming the flags
	for _, tc := range [][2]string{{cert, ""}, {"", key}} {
		_, err := tlsConfigFromFlags(tc[0], tc[1], "pg")
		if err == nil {
			t.Fatalf("cert=%q key=%q: want error", tc[0], tc[1])
		}
		if !strings.Contains(err.Error(), "--pg-tls-cert") || !strings.Contains(err.Error(), "--pg-tls-key") {
			t.Errorf("error %q does not name both flags", err)
		}
	}
	if _, err := tlsConfigFromFlags("", key, "api"); err == nil || !strings.Contains(err.Error(), "--api-tls-cert") {
		t.Errorf("api flag prefix not used in error: %v", err)
	}

	// both set: a usable server config
	cfg, err = tlsConfigFromFlags(cert, key, "pg")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("cfg = %+v, want one certificate", cfg)
	}

	// unreadable files surface an error
	if _, err := tlsConfigFromFlags(cert+".missing", key, "pg"); err == nil {
		t.Fatal("missing cert file: want error")
	}
}

func TestUIURLScheme(t *testing.T) {
	if got := uiURL(":7070", false); got != "http://localhost:7070/ui/" {
		t.Errorf("uiURL(:7070, false) = %q", got)
	}
	if got := uiURL(":7070", true); got != "https://localhost:7070/ui/" {
		t.Errorf("uiURL(:7070, true) = %q", got)
	}
}
