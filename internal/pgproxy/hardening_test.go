package pgproxy

import (
	"testing"
	"time"
)

// A Proxy built as a bare struct literal gets the same protections as New():
// every zero field falls back to its default. Only a negative cap disables it.
func TestZeroValueProxyUsesDefaults(t *testing.T) {
	var p Proxy
	for _, tt := range []struct {
		name      string
		got, want time.Duration
	}{
		{"DialTimeout", p.dialTimeout(), defaultDialTimeout},
		{"StartupTimeout", p.startupTimeout(), defaultStartupTimeout},
		{"IdleTimeout", p.idleTimeout(), defaultIdleTimeout},
	} {
		if tt.got != tt.want {
			t.Errorf("zero %s -> %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if got := p.maxConns(); got != defaultMaxConns {
		t.Errorf("zero MaxConns -> %d, want %d", got, defaultMaxConns)
	}
	p.MaxConns = -1
	if p.maxConns() != 0 {
		t.Errorf("negative MaxConns -> %d, want 0 = disabled", p.maxConns())
	}

	// And it routes like one built by New.
	srv := startPGServer(t, keyedSession(1, []byte{1, 2, 3, 4}))
	addr := startProxyWith(t, &Proxy{Resolver: fakeResolver{"pr-1": srv.addr}})
	openSession(t, dialProxy(t, addr), "pr-1")
}
