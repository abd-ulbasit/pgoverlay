package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// A seed that fails on the source's configuration (the README's first-run
// pg_hba gotcha) is a 422 carrying the tool's message, not an opaque 500 that
// only branchd's log explains. The password never appears.
func TestSeedFailureIs422WithCause(t *testing.T) {
	ts, d := newTestServer(t)
	d.seedErr = errors.New(`helper exited 1: pg_basebackup: error: connection to server at "db.internal" (10.0.0.5), port 5432 failed: FATAL:  no pg_hba.conf entry for replication connection from host "10.0.0.9", user "postgres"`)
	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Port: 5432, User: "postgres", Password: "s3cret-pw",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("failing seed = %d %s, want 422", code, body)
	}
	if !strings.Contains(string(body), "no pg_hba.conf entry for replication connection") {
		t.Errorf("422 body does not name the cause: %s", body)
	}
	if strings.Contains(string(body), "s3cret-pw") {
		t.Errorf("422 body leaks the source password: %s", body)
	}
	// the failed source does not block a retry once the source is fixed
	d.seedErr = nil
	if code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Port: 5432, User: "postgres", Password: "s3cret-pw",
	}); code != http.StatusCreated {
		t.Fatalf("retry after fixing the source = %d %s, want 201", code, body)
	}
}

// A failing masking script is the source's (admin-supplied) SQL at fault: the
// branch create is a 422 naming the script and carrying psql's error.
func TestMaskingFailureIs422WithCause(t *testing.T) {
	ts, d := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "PUT", "/v1/sources/main/mask", []MaskScript{{Name: "emails.sql", SQL: "UPDATE nope SET x = 1"}}); code != http.StatusOK {
		t.Fatalf("put mask: %d %s", code, body)
	}
	d.execErr = errors.New(`exited 1: ERROR:  relation "nope" does not exist`)
	code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("failing masking script = %d %s, want 422", code, body)
	}
	for _, want := range []string{`emails.sql`, `relation \"nope\" does not exist`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("422 body missing %s: %s", want, body)
		}
	}
}

// Duplicate names are a 409 with a plain sentence, not SQLite's constraint
// text.
func TestDuplicateNamesAreClean409(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	cases := []struct {
		method, path string
		body         any
		want         string
	}{
		{"POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}, "a live branch with that name already exists"},
		{"POST", "/v1/tokens", CreateTokenRequest{Name: "ci", Role: registry.RoleViewer}, "a token with that name already exists"},
		{"POST", "/v1/sources", CreateSourceRequest{Name: "main", Host: "db.internal", Password: "secret"}, "a source with that name already exists"},
	}
	for _, tc := range cases {
		if tc.path != "/v1/sources" {
			if code, body := do(t, ts, testToken, tc.method, tc.path, tc.body); code != http.StatusCreated {
				t.Fatalf("first %s %s = %d %s", tc.method, tc.path, code, body)
			}
		}
		code, body := do(t, ts, testToken, tc.method, tc.path, tc.body)
		if code != http.StatusConflict {
			t.Errorf("duplicate %s %s = %d %s, want 409", tc.method, tc.path, code, body)
			continue
		}
		if !strings.Contains(string(body), tc.want) || strings.Contains(string(body), "constraint") {
			t.Errorf("duplicate %s %s body = %s, want %q without SQLite text", tc.method, tc.path, body, tc.want)
		}
	}
}

// A failing source lookup is an error, not a 200 with default credentials
// that would point a client at the wrong database; a source row that is
// genuinely gone still renders with the defaults.
func TestBranchJSONSurfacesSourceLookupErrors(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(nil, reg, testToken, nil, nil, 0)
	b := &registry.Branch{Name: "pr-1", SourceID: "gone"}

	got, err := srv.branchJSON(b)
	if err != nil {
		t.Fatalf("missing source row: unexpected error %v", err)
	}
	if got.Source != "" || got.User != "postgres" || got.Database != "postgres" {
		t.Fatalf("missing source row rendered %+v, want the defaults", got)
	}

	reg.Close() // every later query fails with a non-NotFound error
	if _, err := srv.branchJSON(b); err == nil {
		t.Fatal("source lookup failure was swallowed")
	}
	rec := httptest.NewRecorder()
	srv.writeBranch(rec, httptest.NewRequest("GET", "/v1/branches/pr-1", nil), http.StatusOK, b)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("writeBranch on a failed lookup = %d %s, want 500", rec.Code, rec.Body)
	}
}

func TestClipMessage(t *testing.T) {
	short := "pg_basebackup: FATAL: password authentication failed"
	if got := clipMessage(short); got != short {
		t.Fatalf("short message changed: %q", got)
	}
	long := "seed source \"main\": pg_dump seed: " + strings.Repeat("é", 4000) + " FATAL: the real cause"
	got := clipMessage(long)
	if len(got) > maxErrorMessage+8 {
		t.Fatalf("clipped length %d, want about %d", len(got), maxErrorMessage)
	}
	if !strings.HasPrefix(got, "seed source \"main\"") || !strings.HasSuffix(got, "FATAL: the real cause") {
		t.Fatalf("clip lost the head or the tail: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("clip split a UTF-8 sequence")
	}
}
