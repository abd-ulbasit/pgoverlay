package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// CLI-09: masking could only be cleared with a hand-made PUT of [].
func TestServerModeClearMask(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotBody = r.Method, r.URL.Path, strings.TrimSpace(string(b))
		w.Write([]byte("[]"))
	}))
	defer ts.Close()
	t.Setenv("PGOVERLAY_TOKEN", "tok")

	out := run(t, "source", "clear-mask", "main", "--server", ts.URL)
	if gotMethod != "PUT" || gotPath != "/v1/sources/main/mask" || gotBody != "[]" {
		t.Fatalf("sent %s %s %q, want PUT /v1/sources/main/mask []", gotMethod, gotPath, gotBody)
	}
	if !strings.Contains(out, `source "main" masking cleared`) {
		t.Fatalf("output %q", out)
	}
}

func TestLocalModeClearMask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PGOVERLAY_HOME", home)
	t.Setenv("PGOVERLAY_SERVER", "")
	reg, err := registry.Open(filepath.Join(home, "pgoverlay.db"))
	if err != nil {
		t.Fatal(err)
	}
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetMaskScripts(src.ID, []registry.MaskScript{{Name: "a.sql", SQL: "SELECT 1"}}); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	run(t, "source", "clear-mask", "main")
	if out := run(t, "source", "get-mask", "main"); out != "" {
		t.Fatalf("get-mask after clear-mask = %q, want nothing", out)
	}
	// set-mask still needs at least one file: a forgotten argument must not
	// silently unmask branches
	if _, err := runErr(t, "source", "set-mask", "main"); err == nil {
		t.Fatal("set-mask with no FILE succeeded")
	}
}

// CLI-10: the error named only the env var, and sources with trust, peer or
// certificate auth could not be added without inventing a password.
func TestSourcePasswordHandling(t *testing.T) {
	var got api.CreateSourceRequest
	var gotRefresh api.RefreshSourceRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/refresh") {
			json.NewDecoder(r.Body).Decode(&gotRefresh)
		} else {
			json.NewDecoder(r.Body).Decode(&got)
		}
		json.NewEncoder(w).Encode(api.Source{Name: "main", State: "ready", Generation: 2})
	}))
	defer ts.Close()
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	t.Setenv("PGPASSWORD", "") // empty counts as unset
	t.Setenv("SRC_PW", "")

	_, err := runErr(t, "source", "add", "main", "--host", "db", "--server", ts.URL)
	wantErr(t, err, "$PGPASSWORD", "--password-env", "--no-password")
	_, err = runErr(t, "source", "add", "main", "--host", "db", "--password-env", "SRC_PW", "--server", ts.URL)
	wantErr(t, err, "$SRC_PW", "--password-env")
	_, err = runErr(t, "source", "refresh", "main", "--server", ts.URL)
	wantErr(t, err, "$PGPASSWORD", "--no-password")
	_, err = runErr(t, "source", "add", "main", "--host", "db", "--password-env", "", "--server", ts.URL)
	wantErr(t, err, "--password-env must name")
	_, err = runErr(t, "source", "add", "main", "--host", "db", "--no-password", "--password-env", "SRC_PW", "--server", ts.URL)
	wantErr(t, err, "no-password", "password-env")

	got.Password = "unset"
	run(t, "source", "add", "main", "--host", "db", "--no-password", "--server", ts.URL)
	if got.Name != "main" || got.Password != "" {
		t.Fatalf("--no-password sent %+v", got)
	}
	gotRefresh.Password = "unset"
	run(t, "source", "refresh", "main", "--no-password", "--server", ts.URL)
	if gotRefresh.Password != "" {
		t.Fatalf("refresh --no-password sent password %q", gotRefresh.Password)
	}

	t.Setenv("SRC_PW", "s3cret")
	run(t, "source", "add", "main", "--host", "db", "--password-env", "SRC_PW", "--server", ts.URL)
	if got.Password != "s3cret" {
		t.Fatalf("password = %q, want the value of $SRC_PW", got.Password)
	}
}
