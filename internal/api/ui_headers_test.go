package api

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func getUI(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	return resp, string(body)
}

// Every UI response forbids framing (no clickjacking of destroy/reset) and
// runs under a CSP that allows only same-origin script and style.
func TestUISecurityHeaders(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, p := range []string{"/ui/", "/ui/app.js", "/ui/app.css"} {
		resp, _ := getUI(t, ts.URL+p)
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"frame-ancestors 'none'", "script-src 'self'", "style-src 'self'", "default-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: CSP allows unsafe-inline: %q", p, csp)
		}
		for k, want := range map[string]string{
			"X-Frame-Options":        "DENY",
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := h.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", p, k, got, want)
			}
		}
	}
}

// The CSP forbids inline code, so the page must not carry any: inline
// <script>/<style> blocks, style= attributes or on*= handlers would be
// silently blocked by the browser.
func TestUIPageHasNoInlineCode(t *testing.T) {
	ts, _ := newTestServer(t)
	_, page := getUI(t, ts.URL+"/ui/")
	if regexp.MustCompile(`<script>|<script\s+[^>]*>[^<\s]`).MatchString(page) {
		t.Error("index.html has an inline <script>")
	}
	if strings.Contains(page, "<style") {
		t.Error("index.html has an inline <style>")
	}
	if regexp.MustCompile(`\s(style|on[a-z]+)=`).MatchString(page) {
		t.Error("index.html has an inline style= or on*= attribute")
	}
	if !strings.Contains(page, `<script src="app.js">`) || !strings.Contains(page, `href="app.css"`) {
		t.Error("index.html does not load app.js / app.css")
	}

	resp, js := getUI(t, ts.URL+"/ui/app.js")
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	if regexp.MustCompile(`\sstyle=`).MatchString(js) {
		t.Error("app.js renders an inline style= attribute, which the CSP blocks")
	}
}

// reset discards every write in a branch, so it asks for confirmation like
// destroy; the token is kept per tab (sessionStorage), not persisted.
func TestUIDestructiveActionsConfirmAndTokenNotPersisted(t *testing.T) {
	ts, _ := newTestServer(t)
	_, js := getUI(t, ts.URL+"/ui/app.js")
	reset := strings.Index(js, `btn.dataset.act === "reset"`)
	resetCall := strings.Index(js, "/reset`")
	if reset < 0 || resetCall < 0 {
		t.Fatal("app.js has no reset action")
	}
	if !strings.Contains(js[reset:resetCall], "confirm(") {
		t.Error("reset fires without a confirm() dialog")
	}
	if strings.Contains(js, "localStorage.setItem") {
		t.Error("the token is persisted in localStorage")
	}
	if !strings.Contains(js, "sessionStorage.setItem") {
		t.Error("the token is not kept in sessionStorage")
	}
}
