package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The embedded web UI is a static page: served without auth (it holds no
// secrets), while every /v1 API call it makes still needs the bearer token.
func TestUIServedWithoutAuth(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/ui/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ code=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type=%q want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "pgbranch") {
		t.Fatalf("UI page does not mention pgbranch:\n%.200s", body)
	}

	// API routes are not exempted by the UI route
	if code, _ := do(t, ts, "", "GET", "/v1/branches", nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/branches without token: code=%d want 401", code)
	}
}
