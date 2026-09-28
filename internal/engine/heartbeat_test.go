package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// slowMaskDriver runs onMask inside the masking exec (a long-running user
// masking script), giving the test a hook to run reconcile mid-saga.
type slowMaskDriver struct {
	*fakeDriver
	onMask func()
}

func (d *slowMaskDriver) Exec(ctx context.Context, id string, cmd []string) error {
	if len(cmd) > 0 && cmd[0] == "psql" && strings.Contains(cmd[len(cmd)-1], "slow-mask") && d.onMask != nil {
		d.onMask()
	}
	return d.fakeDriver.Exec(ctx, id, cmd)
}

// LIFECYCLE-07/09: a masking script that runs longer than the stuck timeout
// must not get the branch (or, in a freeze, its parent) failed by reconcile
// while the saga is alive — the saga's heartbeat keeps both rows fresh.
//
// The "slow" script sleeps 600ms against a 300ms stuck timeout, with a 50ms
// heartbeat.
func TestHeartbeatKeepsSlowSagaAliveAcrossReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps ~1s")
	}
	for _, tc := range []struct {
		name   string
		create func(e *Engine) error
		rows   []string
	}{
		{"create", func(e *Engine) error {
			_, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
			return err
		}, []string{"pr-1"}},
		{"freeze", func(e *Engine) error {
			if _, err := e.CreateBranch(context.Background(), "p", "main", 0); err != nil {
				return err
			}
			_, err := e.CreateBranchFrom(context.Background(), "c", "p", 0)
			return err
		}, []string{"p", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &slowMaskDriver{fakeDriver: newFake()}
			e, r := testEngine(t, d, WithHeartbeatInterval(50*time.Millisecond))
			src := readySource(t, r)
			if err := r.SetMaskScripts(src.ID, []registry.MaskScript{{Name: "slow.sql", SQL: "SELECT 'slow-mask'"}}); err != nil {
				t.Fatal(err)
			}
			var taken ReconcilePlan
			var reconciled bool
			d.onMask = func() {
				if tc.name == "freeze" && !reconciled {
					// only the child's masking is interesting (the parent's
					// own create masked before any freeze started)
					if _, err := r.GetBranchByName("c"); err != nil {
						return
					}
				}
				time.Sleep(600 * time.Millisecond)
				var err error
				taken, err = e.ApplyReconcile(context.Background(), time.Now(), 300*time.Millisecond)
				if err != nil {
					t.Errorf("reconcile: %v", err)
				}
				reconciled = true
			}
			if err := tc.create(e); err != nil {
				t.Fatalf("saga failed: %v (reconcile took %+v)", err, taken.Actions)
			}
			if !reconciled {
				t.Fatal("reconcile hook never ran")
			}
			for _, a := range taken.Actions {
				if a.Kind == ActionFailStuck {
					t.Fatalf("reconcile failed a live saga's row: %+v", a)
				}
			}
			for _, name := range tc.rows {
				if b, _ := r.GetBranchByName(name); b.State != registry.BranchReady {
					t.Fatalf("%s state=%s want ready", name, b.State)
				}
			}
		})
	}
}
