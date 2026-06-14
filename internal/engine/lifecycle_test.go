package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// TestCreateBranchQuotaExceeded verifies --max-branches: with a cap of 2 the
// third live branch is refused with ErrQuotaExceeded, and after one is
// destroyed a create succeeds again (the count is of live, not historical,
// branches).
func TestCreateBranchQuotaExceeded(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithMaxBranches(2))
	readySource(t, r)

	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatalf("create pr-1: %v", err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-2", "main", 0); err != nil {
		t.Fatalf("create pr-2: %v", err)
	}
	// third create is at the cap -> refused
	_, err := e.CreateBranch(context.Background(), "pr-3", "main", 0)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("create pr-3 err=%v, want ErrQuotaExceeded", err)
	}
	// the refused create provisions nothing
	if _, err := r.GetBranchByName("pr-3"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("refused create left a row: %v", err)
	}
	// freeing a slot lets a create through again
	if err := e.DestroyBranch(context.Background(), "pr-1"); err != nil {
		t.Fatalf("destroy pr-1: %v", err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-3", "main", 0); err != nil {
		t.Fatalf("create pr-3 after freeing a slot: %v", err)
	}
}

// TestCreateBranchFromQuotaExceeded verifies the cap is enforced on the
// branch-from-branch (overlay freeze) path too.
func TestCreateBranchFromQuotaExceeded(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithMaxBranches(1))
	readySource(t, r)

	if _, err := e.CreateBranch(context.Background(), "parent", "main", 0); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// the single slot is taken; a child create is refused before any freeze
	_, err := e.CreateBranchFrom(context.Background(), "child", "parent", 0)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("CreateBranchFrom err=%v, want ErrQuotaExceeded", err)
	}
	if _, err := r.GetBranchByName("child"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("refused from-branch left a row: %v", err)
	}
}

// TestMaxBranchesZeroUnlimited confirms the default (0) imposes no cap.
func TestMaxBranchesZeroUnlimited(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithMaxBranches(0))
	readySource(t, r)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if _, err := e.CreateBranch(context.Background(), n, "main", 0); err != nil {
			t.Fatalf("create %q under unlimited cap: %v", n, err)
		}
	}
}

func approxExpires(t *testing.T, expiresAt string, want time.Duration) {
	t.Helper()
	if want <= 0 {
		if expiresAt != "" {
			t.Fatalf("ExpiresAt=%q want empty (no expiry)", expiresAt)
		}
		return
	}
	if expiresAt == "" {
		t.Fatalf("ExpiresAt empty, want ~%s out", want)
	}
	got, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("ExpiresAt=%q: %v", expiresAt, err)
	}
	target := time.Now().Add(want)
	if diff := got.Sub(target); diff < -2*time.Minute || diff > 2*time.Minute {
		t.Fatalf("ExpiresAt=%s want ~%s out (%s)", got, want, target)
	}
}

// TestTTLPolicyDefaultApplied: a no-TTL create with --default-ttl=1h expires ~1h out.
func TestTTLPolicyDefaultApplied(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithTTLPolicy(time.Hour, 0))
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	approxExpires(t, b.ExpiresAt, time.Hour)
}

// TestTTLPolicyMaxCaps: a create requesting 24h with --max-ttl=2h is capped to ~2h.
func TestTTLPolicyMaxCaps(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithTTLPolicy(0, 2*time.Hour))
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	approxExpires(t, b.ExpiresAt, 2*time.Hour)
}

// TestTTLPolicyWithinMaxUnchanged: a requested TTL below the cap is left alone.
func TestTTLPolicyWithinMaxUnchanged(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithTTLPolicy(0, 8*time.Hour))
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	approxExpires(t, b.ExpiresAt, time.Hour)
}

// TestTTLPolicyDefaultsZeroUnchanged: with no policy, a no-TTL create never
// expires and an explicit TTL is honored verbatim (regression guard).
func TestTTLPolicyDefaultsZeroUnchanged(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d) // no WithTTLPolicy
	readySource(t, r)

	b0, err := e.CreateBranch(context.Background(), "pr-0", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	approxExpires(t, b0.ExpiresAt, 0) // never expires

	b1, err := e.CreateBranch(context.Background(), "pr-1", "main", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	approxExpires(t, b1.ExpiresAt, 3*time.Hour)
}

// TestTTLPolicyCapsDefaultBranchFrom verifies the policy also applies on the
// branch-from-branch path (default applied when the child requests no TTL).
func TestTTLPolicyDefaultBranchFrom(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithTTLPolicy(time.Hour, 0))
	readySource(t, r)

	if _, err := e.CreateBranch(context.Background(), "parent", "main", 0); err != nil {
		t.Fatal(err)
	}
	child, err := e.CreateBranchFrom(context.Background(), "child", "parent", 0)
	if err != nil {
		t.Fatal(err)
	}
	approxExpires(t, child.ExpiresAt, time.Hour)
}
