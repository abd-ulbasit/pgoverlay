package registry

import (
	"context"
	"testing"
)

func TestActorString(t *testing.T) {
	cases := []struct {
		actor Actor
		want  string
	}{
		{Actor{Name: "ci", Role: "operator"}, "ci (operator)"},
		{Actor{Name: "root", Role: "admin"}, "root (admin)"},
		{Actor{Name: "norole"}, "norole"}, // name with no role formats bare
		{Actor{}, SystemActor},            // empty actor formats as system
	}
	for _, c := range cases {
		if got := c.actor.String(); got != c.want {
			t.Errorf("Actor%+v.String()=%q want %q", c.actor, got, c.want)
		}
	}
}

func TestActorRoundTripContext(t *testing.T) {
	a := Actor{Name: "alice", Role: RoleAdmin}
	ctx := WithActor(context.Background(), a)
	if got := ActorFromContext(ctx); got != a {
		t.Fatalf("ActorFromContext=%+v want %+v", got, a)
	}
	// no actor on a bare context resolves to the zero actor / system string
	if got := ActorFromContext(context.Background()); got != (Actor{}) {
		t.Fatalf("bare ctx actor=%+v want zero", got)
	}
	if got := actorString(context.Background()); got != SystemActor {
		t.Fatalf("bare ctx actorString=%q want %q", got, SystemActor)
	}
}

// seedReadyBranch creates a source and a branch advanced to ready, returning
// the branch.
func seedReadyBranch(t *testing.T, r *Registry, name string) *Branch {
	t.Helper()
	s := &Source{Name: "src-" + name, PGVersion: "17", Volume: "v-" + name}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	b := &Branch{Name: name, SourceID: s.ID, RWVolume: "rw-" + name}
	if err := r.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkBranchReady(b.ID, "cid-"+name, "127.0.0.1", 5400); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTransitionRecordsActorFromContext(t *testing.T) {
	r := openTest(t)
	b := seedReadyBranch(t, r, "pr-actor")
	ctx := WithActor(context.Background(), Actor{Name: "deploy-bot", Role: RoleOperator})
	if err := r.TransitionBranchCtx(ctx, b.ID, BranchDestroying, "destroy requested"); err != nil {
		t.Fatal(err)
	}
	var actor, reason string
	if err := r.db.QueryRow(`SELECT actor, reason FROM transitions WHERE entity_id=? ORDER BY id DESC LIMIT 1`, b.ID).
		Scan(&actor, &reason); err != nil {
		t.Fatal(err)
	}
	if actor != "deploy-bot (operator)" {
		t.Fatalf("actor=%q want %q", actor, "deploy-bot (operator)")
	}
	if reason != "destroy requested" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestTransitionWithoutActorRecordsSystem(t *testing.T) {
	r := openTest(t)
	b := seedReadyBranch(t, r, "pr-sys")
	// no actor on the context (a reconcile/daemon-initiated transition)
	if err := r.TransitionBranchCtx(context.Background(), b.ID, BranchDestroying, "reconcile: stuck"); err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := r.db.QueryRow(`SELECT actor FROM transitions WHERE entity_id=? ORDER BY id DESC LIMIT 1`, b.ID).
		Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != SystemActor {
		t.Fatalf("actor=%q want %q", actor, SystemActor)
	}
	// the back-compat no-ctx wrapper also records the system actor
	if err := r.TransitionBranch(b.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRow(`SELECT actor FROM transitions WHERE entity_id=? ORDER BY id DESC LIMIT 1`, b.ID).
		Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if actor != SystemActor {
		t.Fatalf("wrapper actor=%q want %q", actor, SystemActor)
	}
}

func TestCreateBranchCtxRecordsActor(t *testing.T) {
	r := openTest(t)
	s := &Source{Name: "src-c", PGVersion: "17", Volume: "v-c"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	ctx := WithActor(context.Background(), Actor{Name: "alice", Role: RoleAdmin})
	b := &Branch{Name: "pr-create", SourceID: s.ID, RWVolume: "rw-c"}
	if err := r.CreateBranchCtx(ctx, b); err != nil {
		t.Fatal(err)
	}
	var actor, to string
	if err := r.db.QueryRow(`SELECT actor, to_state FROM transitions WHERE entity_id=? ORDER BY id ASC LIMIT 1`, b.ID).
		Scan(&actor, &to); err != nil {
		t.Fatal(err)
	}
	if actor != "alice (admin)" || to != string(BranchCreating) {
		t.Fatalf("create journal actor=%q to=%q", actor, to)
	}
}

func TestBranchHistory(t *testing.T) {
	r := openTest(t)
	b := seedReadyBranch(t, r, "pr-hist")
	// a destroy by a named operator
	ctx := WithActor(context.Background(), Actor{Name: "ci", Role: RoleOperator})
	if err := r.TransitionBranchCtx(ctx, b.ID, BranchDestroying, "destroy requested"); err != nil {
		t.Fatal(err)
	}
	if err := r.TransitionBranchCtx(ctx, b.ID, BranchDestroyed, ""); err != nil {
		t.Fatal(err)
	}
	hist, err := r.BranchHistory("pr-hist")
	if err != nil {
		t.Fatal(err)
	}
	// created (system, no ctx via CreateBranch) -> ready (system) -> destroying (ci) -> destroyed (ci)
	if len(hist) != 4 {
		t.Fatalf("history len=%d want 4: %+v", len(hist), hist)
	}
	if hist[0].ToState != string(BranchCreating) || hist[0].FromState != "" {
		t.Fatalf("first entry %+v", hist[0])
	}
	if hist[len(hist)-1].ToState != string(BranchDestroyed) || hist[len(hist)-1].Actor != "ci (operator)" {
		t.Fatalf("last entry %+v", hist[len(hist)-1])
	}
	// ordering is oldest-first
	if hist[2].ToState != string(BranchDestroying) || hist[2].Actor != "ci (operator)" {
		t.Fatalf("destroying entry %+v", hist[2])
	}
}

func TestBranchHistoryUnknownName(t *testing.T) {
	r := openTest(t)
	if _, err := r.BranchHistory("nope"); err != ErrNotFound {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestLookupAPITokenActor(t *testing.T) {
	r := openTest(t)
	plaintext, err := r.CreateAPIToken("ci-token", RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	name, role, ok := r.LookupAPITokenActor(plaintext)
	if !ok || name != "ci-token" || role != RoleOperator {
		t.Fatalf("LookupAPITokenActor=%q,%q,%v", name, role, ok)
	}
	if _, _, ok := r.LookupAPITokenActor("bogus"); ok {
		t.Fatal("unknown token resolved")
	}
	if _, _, ok := r.LookupAPITokenActor(""); ok {
		t.Fatal("empty token resolved")
	}
}
