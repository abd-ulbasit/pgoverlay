package pgbranchconnect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stub serves GET /v1/branches/{name}, echoing the supplied wireBranch and
// recording the Authorization header + requested name.
func stub(t *testing.T, w wireBranch) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotAuth, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotName = strings.TrimPrefix(r.URL.Path, "/v1/branches/")
		if w.Name == "" { // simulate not found
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(rw).Encode(w)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuth, &gotName
}

func TestResolveRotatedPassword(t *testing.T) {
	srv, auth, name := stub(t, wireBranch{
		Name: "feat-login", Host: "10.0.0.5", Port: 5432, User: "app",
		Password: "rot123", Database: "appdb", ProxyDatabase: "appdb@feat-login",
	})
	res, err := Resolve(context.Background(), Options{
		Server: srv.URL, Token: "tok", Ref: "feat/Login", ProxyHost: "proxy.example.com:6432",
	})
	if err != nil {
		t.Fatal(err)
	}
	if *auth != "Bearer tok" {
		t.Errorf("auth = %q", *auth)
	}
	if *name != "feat-login" {
		t.Errorf("requested name = %q, want sanitized feat-login", *name)
	}
	if res.DSN != "postgres://app:rot123@10.0.0.5:5432/appdb" {
		t.Errorf("DSN = %q", res.DSN)
	}
	if res.ProxyDSN != "postgres://app:rot123@proxy.example.com:6432/appdb@feat-login" {
		t.Errorf("ProxyDSN = %q", res.ProxyDSN)
	}
}

func TestResolveInheritModeRequiresPassword(t *testing.T) {
	srv, _, _ := stub(t, wireBranch{
		Name: "main-stable", Host: "h", Port: 5432, User: "postgres",
		Database: "postgres", ProxyDatabase: "postgres@main-stable", // no Password
	})
	t.Setenv("PGPASSWORD", "")
	if _, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "main-stable"}); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Fatalf("err = %v, want inherit-mode no-password error", err)
	}
	// supplied password is used in inherit mode
	res, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "main-stable", Password: "fromenv"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.DSN, ":fromenv@") {
		t.Errorf("DSN = %q, want supplied password", res.DSN)
	}
}

func TestResolveNotFound(t *testing.T) {
	srv, _, _ := stub(t, wireBranch{}) // empty Name => 404
	if _, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found", err)
	}
}

func TestResolveValidation(t *testing.T) {
	if _, err := Resolve(context.Background(), Options{Token: "t", Branch: "b"}); err == nil {
		t.Error("want error without Server")
	}
	if _, err := Resolve(context.Background(), Options{Server: "http://x", Branch: "b"}); err == nil {
		t.Error("want error without Token")
	}
	if _, err := Resolve(context.Background(), Options{Server: "http://x", Token: "t"}); err == nil {
		t.Error("want error without Branch or Ref")
	}
}

func TestSanitizeRef(t *testing.T) {
	cases := map[string]string{
		"feat/Login": "feat-login", "FIX--x//y!": "fix-x-y", "-/-": "",
		strings.Repeat("a", 60): strings.Repeat("a", 41),
	}
	for in, want := range cases {
		if got := SanitizeRef(in); got != want {
			t.Errorf("SanitizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}
