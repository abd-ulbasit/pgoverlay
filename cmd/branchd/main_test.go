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

// resolveStorage validates the --runtime/--kube-storage/--cow triangle and
// returns the effective cow backend (--kube-storage csi FORCES the csi
// backend; no separate --cow csi needed).
func TestResolveStorage(t *testing.T) {
	ok := []struct {
		name string
		o    storageOptions
		want string
	}{
		{"docker defaults", storageOptions{runtime: "docker", kubeStorage: "hostpath", cowFlag: "overlay"}, "overlay"},
		{"docker zfs", storageOptions{runtime: "docker", kubeStorage: "hostpath", cowFlag: "zfs", cowSet: true}, "zfs"},
		{"kube hostpath", storageOptions{runtime: "kube", kubeStorage: "hostpath", cowFlag: "overlay", kubeNode: "n1"}, "overlay"},
		{"kube csi forces csi backend", storageOptions{runtime: "kube", kubeStorage: "csi", cowFlag: "overlay", storageClass: "sc"}, "csi"},
		{"kube csi explicit cow csi", storageOptions{runtime: "kube", kubeStorage: "csi", cowFlag: "csi", cowSet: true, storageClass: "sc"}, "csi"},
		{"kube csi no node needed", storageOptions{runtime: "kube", kubeStorage: "csi", cowFlag: "overlay", storageClass: "sc", snapshotClass: "snap", volumeSize: "20Gi"}, "csi"},
	}
	for _, tc := range ok {
		got, err := resolveStorage(tc.o)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: resolveStorage = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	bad := []struct {
		name    string
		o       storageOptions
		errWant string
	}{
		{"csi on docker", storageOptions{runtime: "docker", kubeStorage: "csi", cowFlag: "overlay", storageClass: "sc"}, "--runtime kube"},
		{"csi without storage class", storageOptions{runtime: "kube", kubeStorage: "csi", cowFlag: "overlay"}, "--csi-storage-class"},
		{"csi with cow zfs", storageOptions{runtime: "kube", kubeStorage: "csi", cowFlag: "zfs", cowSet: true, storageClass: "sc"}, "--cow"},
		{"cow csi without csi storage", storageOptions{runtime: "kube", kubeStorage: "hostpath", cowFlag: "csi", cowSet: true, kubeNode: "n1"}, "--kube-storage csi"},
		{"cow csi on docker", storageOptions{runtime: "docker", kubeStorage: "hostpath", cowFlag: "csi", cowSet: true}, "--kube-storage csi"},
		{"kube hostpath without node", storageOptions{runtime: "kube", kubeStorage: "hostpath", cowFlag: "overlay"}, "--kube-node"},
		{"storage class without csi", storageOptions{runtime: "kube", kubeStorage: "hostpath", cowFlag: "overlay", kubeNode: "n1", storageClass: "sc"}, "--kube-storage csi"},
		{"snapshot class without csi", storageOptions{runtime: "docker", kubeStorage: "hostpath", cowFlag: "overlay", snapshotClass: "snap"}, "--kube-storage csi"},
		{"volume size without csi", storageOptions{runtime: "docker", kubeStorage: "hostpath", cowFlag: "overlay", volumeSize: "20Gi"}, "--kube-storage csi"},
		{"unknown storage mode", storageOptions{runtime: "kube", kubeStorage: "nfs", cowFlag: "overlay", kubeNode: "n1"}, "--kube-storage"},
		{"unknown cow backend", storageOptions{runtime: "docker", kubeStorage: "hostpath", cowFlag: "btrfs", cowSet: true}, "cow backend"},
		{"unknown runtime", storageOptions{runtime: "podman", kubeStorage: "hostpath", cowFlag: "overlay"}, "--runtime"},
	}
	for _, tc := range bad {
		_, err := resolveStorage(tc.o)
		if err == nil {
			t.Errorf("%s: want error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.errWant) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.errWant)
		}
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
