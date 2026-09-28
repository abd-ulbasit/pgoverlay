package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// TestRotatedBranchRecoversFromKeyLoss (issue #9) against real docker: a
// rotated branch whose stored password the at-rest key can no longer decrypt
// stays listable, and a reset mints a password that actually authenticates,
// after which destroy works. Names are rotk- prefixed for parallel-run safety.
func TestRotatedBranchRecoversFromKeyLoss(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	mustExec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key);
		INSERT INTO accounts SELECT i FROM generate_series(1,10) i`)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	newKey := func() []byte {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	if err := r.SetSecretKey(newKey()); err != nil {
		t.Fatal(err)
	}
	e := New(r, d, "postgres:17", WithCredentialRotation())

	src := &registry.Source{Name: "rotk-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })
	if _, err := e.CreateBranch(ctx, "rotk-pr-1", "rotk-main", 0); err != nil {
		t.Fatal(err)
	}
	destroyed := false
	t.Cleanup(func() {
		if !destroyed {
			e.DestroyBranch(context.Background(), "rotk-pr-1")
		}
	})

	// the at-rest key is lost and replaced
	if err := r.SetSecretKey(newKey()); err != nil {
		t.Fatal(err)
	}
	b, err := r.GetBranchByName("rotk-pr-1")
	if err != nil || !b.PasswordUnavailable {
		t.Fatalf("after key loss: %+v err=%v, want PasswordUnavailable", b, err)
	}
	if _, err := r.ListLiveBranches(); err != nil {
		t.Fatalf("list after key loss: %v", err)
	}
	if _, err := e.PlanReconcile(ctx, time.Now(), 10*time.Minute); err != nil {
		t.Fatalf("reconcile plan after key loss: %v", err)
	}

	b2, err := e.ResetBranch(ctx, "rotk-pr-1")
	if err != nil {
		t.Fatalf("reset after key loss: %v", err)
	}
	if b2.PasswordUnavailable || !hex32.MatchString(b2.Password) {
		t.Fatalf("reset: %+v, want a fresh readable password", b2)
	}
	conn := fmt.Sprintf("postgres://postgres:%s@localhost:%d/postgres", b2.Password, b2.Port)
	if n := mustQueryInt(t, ctx, conn, `SELECT count(*) FROM accounts`); n != 10 {
		t.Fatalf("branch rows after reset = %d", n)
	}
	if err := e.DestroyBranch(ctx, "rotk-pr-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	destroyed = true
}
