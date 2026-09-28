package runtime_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// TestKubeReconcileRestartsDeletedBranchPod (PGOVERLAY_K8S_IT=1, kind):
// branch pods are bare Pods, so a deleted, drained or evicted pod never comes
// back by itself. Reconcile must report the ready branch as drift, recreate
// the pod on the branch's hostPath volume, record the new pod's address, and
// the branch's writes must survive.
func TestKubeReconcileRestartsDeletedBranchPod(t *testing.T) {
	drv, cs, cfg := kubeIT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	srcIP := startSourcePod(t, ctx, drv, cs)
	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	e := engine.New(r, drv, "postgres:17")
	src := &registry.Source{Name: "k8s-drift", PGVersion: "17", ConnHost: srcIP, ConnPort: 5432, ConnUser: "postgres"}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := e.RemoveSource(ctx, "k8s-drift"); err != nil {
			t.Errorf("remove source: %v", err)
		}
	})
	b, err := e.CreateBranch(ctx, "k8s-drift-1", "k8s-drift", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.DestroyBranch(ctx, "k8s-drift-1"); err != nil {
			t.Errorf("destroy k8s-drift-1: %v", err)
		}
	})
	{
		port := forwardPort(t, cfg, cs, b.ContainerID)
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@127.0.0.1:%d/postgres", port))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `CREATE TABLE kept(x int); INSERT INTO kept VALUES (42); CHECKPOINT`); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	}

	// the pod goes away behind pgoverlay's back (kubectl delete / drain)
	if err := drv.StopRemove(ctx, b.ContainerID); err != nil {
		t.Fatal(err)
	}
	plan, err := e.PlanReconcile(ctx, time.Now(), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !planHas(plan, engine.ActionRestartBranch, "k8s-drift-1") {
		t.Fatalf("doctor misses the deleted branch pod: %+v", plan.Actions)
	}
	taken, err := e.ApplyReconcile(ctx, time.Now(), 10*time.Minute)
	if err != nil || !planHas(taken, engine.ActionRestartBranch, "k8s-drift-1") {
		t.Fatalf("reconcile: %+v, %v", taken.Actions, err)
	}
	nb, err := r.GetBranchByName("k8s-drift-1")
	if err != nil || nb.State != registry.BranchReady {
		t.Fatalf("after reconcile: %+v, %v", nb, err)
	}
	info, err := drv.Inspect(ctx, nb.ContainerID)
	if err != nil || !info.Running || info.Host != nb.Host {
		t.Fatalf("recreated pod %+v, %v; registry host %q", info, err, nb.Host)
	}
	if n := queryInt(t, ctx, forwardPort(t, cfg, cs, nb.ContainerID), `SELECT x FROM kept`); n != 42 {
		t.Fatalf("kept = %d after the pod was recreated: the branch's writes were lost", n)
	}
}

func planHas(p engine.ReconcilePlan, kind engine.ActionKind, target string) bool {
	for _, a := range p.Actions {
		if a.Kind == kind && a.Target == target {
			return true
		}
	}
	return false
}
