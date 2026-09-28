package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// doRaw sends an authenticated request with a raw (possibly invalid) body.
func doRaw(t *testing.T, ts *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

// A misspelled field used to be dropped silently ({"ttl":30} created a branch
// that never expires); it is now a 400 that names the field.
func TestDecodeRejectsUnknownFields(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	code, body := doRaw(t, ts, "POST", "/v1/branches", `{"name":"pr-1","source":"main","ttl":30}`)
	if code != http.StatusBadRequest || !strings.Contains(body, `unknown field \"ttl\"`) {
		t.Fatalf("unknown field = %d %s, want 400 naming \"ttl\"", code, body)
	}
	// nothing was created
	if code, _ := doRaw(t, ts, "GET", "/v1/branches/pr-1", ""); code != http.StatusNotFound {
		t.Fatalf("branch created despite the rejected body: GET = %d", code)
	}
	// the same body with the right key works
	if code, body := doRaw(t, ts, "POST", "/v1/branches", `{"name":"pr-1","source":"main","ttl_seconds":30}`); code != http.StatusCreated {
		t.Fatalf("valid body = %d %s", code, body)
	}
}

func TestDecodeRejectsTrailingData(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	for _, body := range []string{
		`{"name":"pr-1","source":"main"} {"name":"pr-2"}`,
		`{"name":"pr-1","source":"main"}}`,
	} {
		if code, resp := doRaw(t, ts, "POST", "/v1/branches", body); code != http.StatusBadRequest {
			t.Errorf("body %q = %d %s, want 400", body, code, resp)
		}
	}
	// trailing whitespace is fine
	if code, resp := doRaw(t, ts, "POST", "/v1/branches", "{\"name\":\"pr-1\",\"source\":\"main\"}\n"); code != http.StatusCreated {
		t.Errorf("body with trailing newline = %d %s, want 201", code, resp)
	}
}

// Bodies are capped before they are buffered: an oversized mask upload is a
// 413, not a multi-gigabyte allocation written into the registry.
func TestDecodeCapsBodySize(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	huge := `[{"name":"m","sql":"` + strings.Repeat("x", maxBodyBytes) + `"}]`
	code, body := doRaw(t, ts, "PUT", "/v1/sources/main/mask", huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d %s, want 413", code, body)
	}
	if code, body := doRaw(t, ts, "GET", "/v1/sources/main/mask", ""); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("mask scripts after rejected upload = %d %s, want []", code, body)
	}
}

// Token names are bounded and "root" (the audit name of the built-in
// PGOVERLAY_TOKEN) is reserved, so a minted token can never be mistaken for
// the env token in the audit log.
func TestCreateTokenValidatesName(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, name := range []string{"root", "has space", "-leading", "sys:tem", strings.Repeat("a", 65)} {
		code, body := do(t, ts, testToken, "POST", "/v1/tokens", CreateTokenRequest{Name: name, Role: registry.RoleViewer})
		if code != http.StatusBadRequest {
			t.Errorf("token name %q = %d %s, want 400", name, code, body)
		}
	}
	for _, name := range []string{"ci", "CI_bot", "deploy.v2", "a-b"} {
		code, body := do(t, ts, testToken, "POST", "/v1/tokens", CreateTokenRequest{Name: name, Role: registry.RoleViewer})
		if code != http.StatusCreated {
			t.Errorf("token name %q = %d %s, want 201", name, code, body)
		}
	}
	// the token endpoint is strict like every other body
	if code, body := doRaw(t, ts, "POST", "/v1/tokens", `{"name":"x","role":"viewer","rol":"admin"}`); code != http.StatusBadRequest || !strings.Contains(body, "rol") {
		t.Errorf("unknown token field = %d %s, want 400 naming it", code, body)
	}
}
