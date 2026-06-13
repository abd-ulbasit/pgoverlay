package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// mintToken creates a token of the given role via the admin REST endpoint
// (using the built-in env admin token) and returns its plaintext.
func mintToken(t *testing.T, ts *httptest.Server, name, role string) string {
	t.Helper()
	code, body := do(t, ts, testToken, "POST", "/v1/tokens", CreateTokenRequest{Name: name, Role: role})
	if code != http.StatusCreated {
		t.Fatalf("create %s token: status %d body %s", role, code, body)
	}
	return mustUnmarshal[CreateTokenResponse](t, body).Token
}

// A viewer token may read but never mutate or manage tokens.
func TestAuthzViewer(t *testing.T) {
	ts, _ := newTestServer(t)
	viewer := mintToken(t, ts, "v1", registry.RoleViewer)

	if code, _ := do(t, ts, viewer, "GET", "/v1/branches", nil); code != http.StatusOK {
		t.Errorf("viewer GET /v1/branches = %d, want 200", code)
	}
	if code, _ := do(t, ts, viewer, "GET", "/v1/sources", nil); code != http.StatusOK {
		t.Errorf("viewer GET /v1/sources = %d, want 200", code)
	}
	// branch create is operator+
	if code, _ := do(t, ts, viewer, "POST", "/v1/branches", CreateBranchRequest{Name: "b", Source: "main"}); code != http.StatusForbidden {
		t.Errorf("viewer POST /v1/branches = %d, want 403", code)
	}
	// token endpoints are admin-only
	if code, _ := do(t, ts, viewer, "POST", "/v1/tokens", CreateTokenRequest{Name: "x", Role: registry.RoleViewer}); code != http.StatusForbidden {
		t.Errorf("viewer POST /v1/tokens = %d, want 403", code)
	}
	if code, _ := do(t, ts, viewer, "GET", "/v1/tokens", nil); code != http.StatusForbidden {
		t.Errorf("viewer GET /v1/tokens = %d, want 403", code)
	}
	if code, _ := do(t, ts, viewer, "DELETE", "/v1/tokens/v1", nil); code != http.StatusForbidden {
		t.Errorf("viewer DELETE /v1/tokens/v1 = %d, want 403", code)
	}
}

// An operator token may mutate branches and read, but not manage tokens/sources.
func TestAuthzOperator(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts) // seed a ready source so branch create reaches the engine
	operator := mintToken(t, ts, "op1", registry.RoleOperator)

	// branch create: operator is authorized (201 — the engine actually creates it)
	if code, body := do(t, ts, operator, "POST", "/v1/branches", CreateBranchRequest{Name: "br", Source: "main"}); code != http.StatusCreated {
		t.Errorf("operator POST /v1/branches = %d (%s), want 201", code, body)
	}
	// reads are allowed
	if code, _ := do(t, ts, operator, "GET", "/v1/branches", nil); code != http.StatusOK {
		t.Errorf("operator GET /v1/branches = %d, want 200", code)
	}
	// token management is admin-only
	if code, _ := do(t, ts, operator, "GET", "/v1/tokens", nil); code != http.StatusForbidden {
		t.Errorf("operator GET /v1/tokens = %d, want 403", code)
	}
	if code, _ := do(t, ts, operator, "POST", "/v1/tokens", CreateTokenRequest{Name: "x", Role: registry.RoleViewer}); code != http.StatusForbidden {
		t.Errorf("operator POST /v1/tokens = %d, want 403", code)
	}
	// source management is admin-only
	if code, _ := do(t, ts, operator, "DELETE", "/v1/sources/main", nil); code != http.StatusForbidden {
		t.Errorf("operator DELETE /v1/sources/main = %d, want 403", code)
	}
}

// An admin token (stored in the table) may do everything, like the env token.
func TestAuthzAdminToken(t *testing.T) {
	ts, _ := newTestServer(t)
	admin := mintToken(t, ts, "adm1", registry.RoleAdmin)

	if code, _ := do(t, ts, admin, "GET", "/v1/branches", nil); code != http.StatusOK {
		t.Errorf("admin GET /v1/branches = %d, want 200", code)
	}
	if code, body := do(t, ts, admin, "POST", "/v1/tokens", CreateTokenRequest{Name: "another", Role: registry.RoleViewer}); code != http.StatusCreated {
		t.Errorf("admin POST /v1/tokens = %d (%s), want 201", code, body)
	}
	if code, _ := do(t, ts, admin, "GET", "/v1/tokens", nil); code != http.StatusOK {
		t.Errorf("admin GET /v1/tokens = %d, want 200", code)
	}
}

// The legacy PGBRANCH_TOKEN env value (testToken) is treated as a built-in
// admin: it can reach every route, including token management.
func TestAuthzLegacyEnvTokenIsAdmin(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/branches", nil},
		{"GET", "/v1/tokens", nil},
	} {
		if code, body := do(t, ts, testToken, tc.method, tc.path, tc.body); code < 200 || code > 299 {
			t.Errorf("env token %s %s = %d (%s), want 2xx", tc.method, tc.path, code, body)
		}
	}
}

// An unknown / unparseable bearer is 401 everywhere.
func TestAuthzUnknownToken401(t *testing.T) {
	ts, _ := newTestServer(t)
	if code, _ := do(t, ts, "deadbeefdeadbeefdeadbeefdeadbeef", "GET", "/v1/branches", nil); code != http.StatusUnauthorized {
		t.Errorf("unknown token GET /v1/branches = %d, want 401", code)
	}
	if code, _ := do(t, ts, "", "GET", "/v1/branches", nil); code != http.StatusUnauthorized {
		t.Errorf("no token GET /v1/branches = %d, want 401", code)
	}
}

// /metrics and /readyz remain unauthenticated (Prometheus & kubelet probes).
func TestAuthzUnauthenticatedProbes(t *testing.T) {
	ts, _ := newTestServer(t)
	if code, _ := do(t, ts, "", "GET", "/metrics", nil); code != http.StatusOK {
		t.Errorf("unauthenticated GET /metrics = %d, want 200", code)
	}
	if code, _ := do(t, ts, "", "GET", "/readyz", nil); code != http.StatusOK {
		t.Errorf("unauthenticated GET /readyz = %d, want 200", code)
	}
	if code, _ := do(t, ts, "", "GET", "/healthz", nil); code != http.StatusOK {
		t.Errorf("unauthenticated GET /healthz = %d, want 200", code)
	}
}
