package api

import (
	"net/http"
	"strings"
	"testing"
)

// Branch responses advertise the router address set with SetProxyEndpoint,
// so clients stop assuming "<API host>:6432"; without it the fields are
// omitted, as from older servers.
func TestBranchResponsesAdvertiseProxyEndpoint(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create branch: code=%d body=%s", code, body)
	}

	_, body := do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
	if strings.Contains(string(body), "proxy_host") || strings.Contains(string(body), "proxy_port") {
		t.Fatalf("unset endpoint still advertised: %s", body)
	}

	srv.SetProxyEndpoint("pgoverlay-proxy.pgoverlay-system", 5433)
	_, body = do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
	b := mustUnmarshal[Branch](t, body)
	if b.ProxyHost != "pgoverlay-proxy.pgoverlay-system" || b.ProxyPort != 5433 {
		t.Fatalf("GET branch proxy endpoint = %q:%d", b.ProxyHost, b.ProxyPort)
	}
	_, body = do(t, ts, testToken, "GET", "/v1/branches", nil)
	if list := mustUnmarshal[[]Branch](t, body); len(list) != 1 || list[0].ProxyPort != 5433 {
		t.Fatalf("list branches = %+v", list)
	}

	// port only: clients keep the API host
	srv.SetProxyEndpoint("", 6432)
	_, body = do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
	if b := mustUnmarshal[Branch](t, body); b.ProxyHost != "" || b.ProxyPort != 6432 || strings.Contains(string(body), "proxy_host") {
		t.Fatalf("port-only endpoint: %s", body)
	}
}
