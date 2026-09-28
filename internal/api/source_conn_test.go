package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// TestCreateSourceUnusableConnectionRejected: connection settings that can
// never seed are a 400 with the reason, not a 500 after a failed helper, and
// leave no source row behind.
func TestCreateSourceUnusableConnectionRejected(t *testing.T) {
	ts, d := newTestServer(t)
	cases := []struct {
		name string
		req  CreateSourceRequest
		want string
	}{
		{"blank host", CreateSourceRequest{Name: "main", Host: "   ", Password: "secret"}, "host"},
		{"port out of range", CreateSourceRequest{Name: "main", Host: "db", Port: 70000, Password: "secret"}, "out of range"},
		{"comma in schema", CreateSourceRequest{Name: "main", Host: "db", Password: "secret",
			Via: registry.SeedViaDump, DumpSchemas: []string{`"a,b"`}}, "comma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, testToken, "POST", "/v1/sources", tc.req)
			if code != http.StatusBadRequest {
				t.Fatalf("code=%d want 400 (body=%s)", code, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body %s should mention %q", body, tc.want)
			}
		})
	}
	if code, body := do(t, ts, testToken, "GET", "/v1/sources", nil); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("sources after rejected creates: code=%d body=%s, want []", code, body)
	}
	if len(d.volumes) != 0 {
		t.Fatalf("rejected creates provisioned volumes: %v", d.volumes)
	}
}
