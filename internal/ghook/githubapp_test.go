package ghook

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKey generates a throwaway RSA key for JWT signing/verification.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func pemPKCS1(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func pemPKCS8(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParseAppPrivateKeyPKCS1AndPKCS8(t *testing.T) {
	key := testKey(t)
	for name, p := range map[string]string{"pkcs1": pemPKCS1(t, key), "pkcs8": pemPKCS8(t, key)} {
		got, err := ParseAppPrivateKey([]byte(p))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !got.Equal(key) {
			t.Errorf("%s: parsed key differs", name)
		}
	}
	if _, err := ParseAppPrivateKey([]byte("not a pem")); err == nil {
		t.Error("garbage PEM: want error")
	}
}

// The app JWT is the hand-rolled RS256 token GitHub expects when minting
// installation tokens: base64url(header).base64url(claims).base64url(sig),
// iss = app id, ~10 minute validity window (iat backdated 60s).
func TestAppJWTShapeAndSignature(t *testing.T) {
	key := testKey(t)
	now := time.Now()
	tok, err := appJWT("12345", key, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3: %q", len(parts), tok)
	}
	decode := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("base64url decode %q: %v", s, err)
		}
		return b
	}
	var header struct{ Alg, Typ string }
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Errorf("header = %+v, want RS256/JWT", header)
	}
	var claims struct {
		Iss      string `json:"iss"`
		Iat, Exp int64
	}
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "12345" {
		t.Errorf("iss = %q, want 12345", claims.Iss)
	}
	if got := now.Unix() - claims.Iat; got != 60 {
		t.Errorf("iat backdate = %ds, want 60", got)
	}
	if got := claims.Exp - now.Unix(); got != 9*60 {
		t.Errorf("exp horizon = %ds, want 540 (9 min)", got)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], decode(parts[2])); err != nil {
		t.Errorf("RS256 signature does not verify: %v", err)
	}
}

// fakeAppAPI serves POST /app/installations/{id}/access_tokens, counting
// mints and capturing the app JWT presented as the bearer.
type fakeAppAPI struct {
	mints     map[string]int // installation id -> mint count
	lastJWT   string
	expiresIn time.Duration // expiry horizon of minted tokens
	srv       *httptest.Server
}

func newFakeAppAPI(t *testing.T) *fakeAppAPI {
	f := &fakeAppAPI{mints: map[string]int{}, expiresIn: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mints[id]++
		f.lastJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"token":      fmt.Sprintf("ghs_inst%s_%d", id, f.mints[id]),
			"expires_at": time.Now().Add(f.expiresIn).UTC().Format(time.RFC3339),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestInstallationTokensAreCachedUntilNearExpiry(t *testing.T) {
	key := testKey(t)
	api := newFakeAppAPI(t)
	auth := NewAppAuth("12345", key, api.srv.URL, api.srv.Client())

	tok1, err := auth.Token(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := auth.Token(t.Context(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != tok2 || api.mints["42"] != 1 {
		t.Fatalf("tokens %q/%q, mints = %d; want one cached mint", tok1, tok2, api.mints["42"])
	}

	// distinct installations mint distinct tokens
	if _, err := auth.Token(t.Context(), 99); err != nil {
		t.Fatal(err)
	}
	if api.mints["99"] != 1 {
		t.Fatalf("mints[99] = %d, want 1", api.mints["99"])
	}

	// tokens expiring within the 5-minute safety buffer are re-minted
	api.expiresIn = 4 * time.Minute
	if _, err := auth.Token(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Token(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if api.mints["7"] != 2 {
		t.Fatalf("mints[7] = %d, want 2 (near-expiry token must re-mint)", api.mints["7"])
	}
}

func TestTokenWithoutInstallationIDFails(t *testing.T) {
	auth := NewAppAuth("12345", testKey(t), "http://127.0.0.1:1", nil)
	if _, err := auth.Token(t.Context(), 0); err == nil {
		t.Fatal("installation id 0: want error (webhook not delivered via the App)")
	}
}

func TestPayloadParsesInstallationID(t *testing.T) {
	var p payload
	if err := json.Unmarshal(fixture(t, "pr_opened.json"), &p); err != nil {
		t.Fatal(err)
	}
	if p.Installation.ID != 4242 {
		t.Fatalf("installation.id = %d, want 4242", p.Installation.ID)
	}
}

// End-to-end in App mode: the delivery's installation.id drives the token
// mint, and the minted installation token (not the app JWT) authenticates
// the comment/status calls.
func TestAppModeUsesInstallationTokenFromPayload(t *testing.T) {
	key := testKey(t)
	pg := newFakePG(t, false)
	gh := newFakeGitHub(t)

	// route /app/... to the mint fake and everything else to the GitHub fake
	app := newFakeAppAPI(t)
	mux := http.NewServeMux()
	mux.Handle("/app/", http.StripPrefix("", app.srv.Config.Handler))
	mux.Handle("/", gh.srv.Config.Handler)
	combined := httptest.NewServer(mux)
	t.Cleanup(combined.Close)

	auth := NewAppAuth("12345", key, combined.URL, combined.Client())
	client := &GitHub{BaseURL: combined.URL, Token: auth.Token, HTTP: combined.Client()}
	deliver(t, newService(Config{}, pg.srv.URL, client), fixture(t, "pr_opened.json"))

	if app.mints["4242"] != 1 {
		t.Fatalf("mints = %v, want exactly one for installation 4242", app.mints)
	}
	wantAuth := "Bearer ghs_inst4242_1"
	if got := gh.lastReq.Header.Get("Authorization"); got != wantAuth {
		t.Fatalf("GitHub call auth = %q, want %q (the installation token)", got, wantAuth)
	}
	if len(gh.statuses) != 2 || len(gh.posted) != 1 {
		t.Fatalf("statuses=%d posted=%d, want 2 statuses and 1 comment", len(gh.statuses), len(gh.posted))
	}
}
