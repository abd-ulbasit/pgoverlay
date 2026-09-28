package api

import (
	"net/http"
	"strings"
	"testing"
)

// POST /v1/sources accepts an optional image override and echoes it; an
// invalid reference is a 400. Sources without one omit the field.
func TestCreateSourceImageOverride(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "geo", Host: "db.internal", PGVersion: "17", Password: "pw", Image: "postgis/postgis:17-3.5",
	})
	if code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	if got := mustUnmarshal[Source](t, body); got.Image != "postgis/postgis:17-3.5" {
		t.Fatalf("image not echoed: %+v", got)
	}
	code, body = do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "bad", Host: "db.internal", PGVersion: "17", Password: "pw", Image: "x; rm -rf /",
	})
	if code != http.StatusBadRequest || !strings.Contains(string(body), "invalid image reference") {
		t.Fatalf("invalid image: code=%d body=%s, want 400", code, body)
	}
	plain := addSource(t, ts)
	if plain.Image != "" {
		t.Fatalf("default source reports image %q", plain.Image)
	}
	if _, body = do(t, ts, testToken, "GET", "/v1/sources", nil); strings.Count(string(body), `"image"`) != 1 {
		t.Fatalf("list should carry image only for the overriding source: %s", body)
	}
}
