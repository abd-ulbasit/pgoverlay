package ghook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/apiclient"
)

const testSecret = "wh-s3cret"

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newService wires a Service to a pgbranch API base URL; gh may be nil.
func newService(cfg Config, pgURL string, gh *GitHub) *Service {
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = testSecret
	}
	if cfg.Source == "" {
		cfg.Source = "main"
	}
	return New(cfg, apiclient.New(pgURL, "tok"), gh, testLogger())
}

// post sends body to /webhook with the given event and signature headers.
func post(t *testing.T, h http.Handler, event, sig string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/webhook", strings.NewReader(string(body)))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestWebhookRejectsBadSignatures(t *testing.T) {
	// pgbranch API that must never be reached
	called := false
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer pg.Close()
	h := newService(Config{}, pg.URL, nil).Handler()

	body := fixture(t, "pr_opened.json")
	cases := []struct {
		name, sig string
	}{
		{"missing", ""},
		{"wrong secret", sign("other-secret", body)},
		{"garbage", "sha256=zzzz"},
		{"truncated", sign(testSecret, body)[:20]},
		{"different body", sign(testSecret, []byte("{}"))},
	}
	for _, tc := range cases {
		if rr := post(t, h, "pull_request", tc.sig, body); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s signature: code=%d want 401", tc.name, rr.Code)
		}
	}
	if called {
		t.Fatal("pgbranch API was called despite invalid signature")
	}
}

func TestWebhookAcceptsValidSignatureAndIgnoresOtherEvents(t *testing.T) {
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected pgbranch call %s %s", r.Method, r.URL.Path)
	}))
	defer pg.Close()
	h := newService(Config{}, pg.URL, nil).Handler()

	body := []byte(`{"zen":"keep it simple"}`)
	if rr := post(t, h, "ping", sign(testSecret, body), body); rr.Code != http.StatusNoContent {
		t.Fatalf("ping event: code=%d want 204", rr.Code)
	}
	// pull_request with an unhandled action is also a no-op
	pr := []byte(`{"action":"labeled","number":7,"repository":{"full_name":"acme/widgets"}}`)
	if rr := post(t, h, "pull_request", sign(testSecret, pr), pr); rr.Code != http.StatusNoContent {
		t.Fatalf("labeled action: code=%d want 204", rr.Code)
	}
}

func TestWebhookMethodAndPathHandling(t *testing.T) {
	h := newService(Config{}, "http://127.0.0.1:1", nil).Handler()

	req := httptest.NewRequest("GET", "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: code=%d want 200", rr.Code)
	}

	req = httptest.NewRequest("GET", "/webhook", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /webhook: code=%d want 405", rr.Code)
	}
}

func TestWebhookRejectsInvalidJSON(t *testing.T) {
	h := newService(Config{}, "http://127.0.0.1:1", nil).Handler()
	body := []byte(`{not json`)
	if rr := post(t, h, "pull_request", sign(testSecret, body), body); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON: code=%d want 400", rr.Code)
	}
}
