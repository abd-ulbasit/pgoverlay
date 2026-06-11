// GitHub App authentication: instead of a static PAT, the service can hold
// an App private key and mint short-lived installation tokens on demand —
// a hand-rolled RS256 JWT (no JWT dependency) exchanged at
// POST /app/installations/{id}/access_tokens. The installation id comes from
// each webhook delivery's payload.

package ghook

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ParseAppPrivateKey parses the App's RSA private key from PEM (GitHub
// downloads PKCS#1 "RSA PRIVATE KEY" files; PKCS#8 is accepted too for keys
// that went through openssl).
func ParseAppPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("app private key: no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("app private key: not PKCS#1 or PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("app private key: %T, want RSA (GitHub Apps sign RS256)", parsed)
	}
	return key, nil
}

// appJWT builds the RS256 app JWT GitHub requires for App-level endpoints:
// iss = app id, iat backdated 60s (clock-skew allowance), exp 9 minutes out
// (GitHub caps at 10).
func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iss": appID,
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// AppAuth mints and caches GitHub App installation tokens. Its Token method
// is a TokenProvider for the GitHub client.
type AppAuth struct {
	appID   string
	key     *rsa.PrivateKey
	baseURL string
	http    *http.Client

	mu    sync.Mutex
	cache map[int64]instToken
}

type instToken struct {
	token   string
	expires time.Time
}

func NewAppAuth(appID string, key *rsa.PrivateKey, baseURL string, hc *http.Client) *AppAuth {
	return &AppAuth{appID: appID, key: key, baseURL: baseURL, http: hc, cache: map[int64]instToken{}}
}

// Token returns an installation access token for installationID, minting a
// fresh one when the cached token is gone or within 5 minutes of expiry.
func (a *AppAuth) Token(ctx context.Context, installationID int64) (string, error) {
	if installationID == 0 {
		return "", errors.New("delivery carries no installation.id — register the webhook on the GitHub App, not as a plain repo webhook")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.cache[installationID]; ok && time.Now().Before(t.expires.Add(-5*time.Minute)) {
		return t.token, nil
	}
	t, err := a.mint(ctx, installationID)
	if err != nil {
		return "", err
	}
	a.cache[installationID] = t
	return t.token, nil
}

// mint exchanges a fresh app JWT for an installation token.
func (a *AppAuth) mint(ctx context.Context, installationID int64) (instToken, error) {
	jwt, err := appJWT(a.appID, a.key, time.Now())
	if err != nil {
		return instToken{}, err
	}
	base := strings.TrimRight(a.baseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", base, installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return instToken{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	hc := a.http
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return instToken{}, fmt.Errorf("mint installation token: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return instToken{}, err
	}
	if resp.StatusCode != http.StatusCreated {
		return instToken{}, fmt.Errorf("mint installation token: HTTP %d: %.200s", resp.StatusCode, data)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return instToken{}, err
	}
	return instToken{token: out.Token, expires: out.ExpiresAt}, nil
}
