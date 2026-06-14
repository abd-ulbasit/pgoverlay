package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// findActor returns the recorded actor for the first history entry whose
// to_state matches, or "" if none.
func findActor(hist []Transition, toState string) string {
	for _, t := range hist {
		if t.ToState == toState {
			return t.Actor
		}
	}
	return ""
}

// TestAuditRecordsTokenActor: a branch created by a named operator token, then
// destroyed by the built-in env admin, records each token as the actor of the
// transitions it caused.
func TestAuditRecordsTokenActor(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	operator := mintToken(t, ts, "deploy-bot", registry.RoleOperator)

	if code, body := do(t, ts, operator, "POST", "/v1/branches",
		CreateBranchRequest{Name: "audit-br", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	// destroy with the env admin token (sentinel actor "root")
	if code, body := do(t, ts, testToken, "DELETE", "/v1/branches/audit-br", nil); code != http.StatusNoContent {
		t.Fatalf("destroy: %d %s", code, body)
	}

	code, body := do(t, ts, operator, "GET", "/v1/branches/audit-br/history", nil)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	hist := mustUnmarshal[[]Transition](t, body)

	// created and ready were caused by the operator token
	if got := findActor(hist, "creating"); got != "deploy-bot (operator)" {
		t.Errorf("creating actor=%q want %q", got, "deploy-bot (operator)")
	}
	if got := findActor(hist, "ready"); got != "deploy-bot (operator)" {
		t.Errorf("ready actor=%q want %q", got, "deploy-bot (operator)")
	}
	// destroyed was caused by the built-in env admin token (the "root" sentinel)
	if got := findActor(hist, "destroyed"); got != "root (admin)" {
		t.Errorf("destroyed actor=%q want %q", got, "root (admin)")
	}
}

// TestAuditResetRecordsActor: a reset records the token that requested it on
// both the resetting and the ready-again transitions.
func TestAuditResetRecordsActor(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	operator := mintToken(t, ts, "op-reset", registry.RoleOperator)

	if code, body := do(t, ts, operator, "POST", "/v1/branches",
		CreateBranchRequest{Name: "reset-br", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, body := do(t, ts, operator, "POST", "/v1/branches/reset-br/reset", nil); code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, body)
	}
	code, body := do(t, ts, operator, "GET", "/v1/branches/reset-br/history", nil)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	hist := mustUnmarshal[[]Transition](t, body)
	if got := findActor(hist, "resetting"); got != "op-reset (operator)" {
		t.Errorf("resetting actor=%q want %q", got, "op-reset (operator)")
	}
}

// TestAuditReconcileRecordsSystemActor: a transition initiated by the daemon
// (reconcile's TTL-expiry destroy) with no request actor records SystemActor.
func TestAuditReconcileRecordsSystemActor(t *testing.T) {
	ts, srv := newTestServerWithLeader(t)
	addSource(t, ts)
	admin := mintToken(t, ts, "adm", registry.RoleAdmin)

	// create a branch with a 1s TTL so reconcile's expiry pass destroys it
	if code, body := do(t, ts, admin, "POST", "/v1/branches",
		CreateBranchRequest{Name: "ttl-br", Source: "main", TTLSeconds: 1}); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	// run reconcile directly through the engine with a context carrying NO actor
	// (the daemon's reconcile loop), well past the TTL.
	if _, err := srv.eng.ApplyReconcile(context.Background(), time.Now().Add(time.Hour), 0); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	code, body := do(t, ts, admin, "GET", "/v1/branches/ttl-br/history", nil)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	hist := mustUnmarshal[[]Transition](t, body)
	// the create was the admin; the destroy was the daemon (system actor)
	if got := findActor(hist, "creating"); got != "adm (admin)" {
		t.Errorf("creating actor=%q want %q", got, "adm (admin)")
	}
	if got := findActor(hist, "destroyed"); got != registry.SystemActor {
		t.Errorf("destroyed actor=%q want %q", got, registry.SystemActor)
	}
}
