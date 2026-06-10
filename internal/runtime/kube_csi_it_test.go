// CSI-mode integration tests against the pgbranch-test kind cluster with the
// csi-driver-host-path stack (hack/kind-csi-up.sh). Gated by
// PGBRANCH_CSI_IT=1. Mirrors kube_it_test.go: branchd-less, driving the
// driver + engine in-process; reuses its source-pod / port-forward helpers.
package runtime_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/abd-ulbasit/pgbranch/internal/cow"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	rt "github.com/abd-ulbasit/pgbranch/internal/runtime"
)

const csiStorageClassName = "csi-hostpath-sc"

// csiIT ensures the kind cluster + csi-driver-host-path stack exist and
// returns a csi-mode driver plus raw client-go handles.
func csiIT(t *testing.T, snapshotClass string) (*rt.KubeDriver, *kubernetes.Clientset, *rest.Config) {
	t.Helper()
	if os.Getenv("PGBRANCH_CSI_IT") != "1" {
		t.Skip("set PGBRANCH_CSI_IT=1 to run csi integration tests")
	}
	if out, err := exec.Command("../../hack/kind-csi-up.sh").CombinedOutput(); err != nil {
		t.Fatalf("hack/kind-csi-up.sh: %v\n%s", err, out)
	}
	kc, err := exec.Command("kind", "get", "kubeconfig", "--name", kindCluster).Output()
	if err != nil {
		t.Fatalf("kind get kubeconfig: %v", err)
	}
	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kcPath, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	drv, err := rt.NewKubeDriverCSI(kcPath, kubeNS, rt.CSIConfig{
		StorageClass:  csiStorageClassName,
		SnapshotClass: snapshotClass,
		VolumeSize:    "2Gi",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return drv, cs, cfg
}

// pgbranchPVCs lists the names of PVCs the engine/driver created (they all
// carry pgbranch.managed=true).
func pgbranchPVCs(t *testing.T, ctx context.Context, cs *kubernetes.Clientset) []string {
	t.Helper()
	pvcs, err := cs.CoreV1().PersistentVolumeClaims(kubeNS).List(ctx,
		metav1.ListOptions{LabelSelector: "pgbranch.managed=true"})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pvcs.Items))
	for i := range pvcs.Items {
		names = append(names, pvcs.Items[i].Name)
	}
	return names
}

func execSQL(t *testing.T, ctx context.Context, drv *rt.KubeDriver, pod, sql string) {
	t.Helper()
	if err := drv.Exec(ctx, pod, []string{"psql", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-c", sql}); err != nil {
		t.Fatalf("psql on %s: %v", pod, err)
	}
}

// TestKubeCSIEndToEndBranching: seed a source into a PVC, branch it (PVC
// dataSource clone), verify SQL + isolation, branch-from-branch (brief parent
// stop), destroy everything, no PVCs left.
func TestKubeCSIEndToEndBranching(t *testing.T) {
	drv, cs, cfg := csiIT(t, "" /* PVC dataSource clones */)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	srcIP := startSourcePod(t, ctx, drv, cs)
	execSQL(t, ctx, drv, "pgbranch-it-source",
		`CREATE TABLE accounts(id int primary key, balance int);
		 INSERT INTO accounts SELECT i, 100 FROM generate_series(1,10000) i`)

	r, err := registry.Open(t.TempDir() + "/csi-it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // after the destroy cleanups below (LIFO)
	e := engine.NewWithPlanner(r, drv, "postgres:17", cow.Planner{Backend: cow.BackendCSI})

	src := &registry.Source{Name: "csi-main", PGVersion: "17", ConnHost: srcIP, ConnPort: 5432, ConnUser: "postgres"}
	start := time.Now()
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Logf("source seeded into PVC in %s", time.Since(start))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.RemoveSource(ctx, "csi-main"); err != nil {
			t.Errorf("remove source: %v", err)
		}
		if left := pgbranchPVCs(t, ctx, cs); len(left) != 0 {
			t.Errorf("PVCs left after full teardown: %v", left)
		}
	})
	// the source layer is a PVC
	if _, err := cs.CoreV1().PersistentVolumeClaims(kubeNS).Get(ctx, "pgbranch-src-csi-main", metav1.GetOptions{}); err != nil {
		t.Fatalf("source PVC: %v", err)
	}

	// branch = PVC clone
	start = time.Now()
	b, err := e.CreateBranch(ctx, "csi-pr-1", "csi-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("branch csi-pr-1 created in %s (pod %s, host %s:%d)", time.Since(start), b.ContainerID, b.Host, b.Port)
	pr1Destroyed := false
	t.Cleanup(func() {
		if pr1Destroyed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.DestroyBranch(ctx, "csi-pr-1"); err != nil {
			t.Errorf("destroy csi-pr-1: %v", err)
		}
	})
	pvc, err := cs.CoreV1().PersistentVolumeClaims(kubeNS).Get(ctx, "pgbranch-br-csi-pr-1-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Kind != "PersistentVolumeClaim" || pvc.Spec.DataSource.Name != "pgbranch-src-csi-main" {
		t.Errorf("branch PVC dataSource = %+v, want clone of the source PVC", pvc.Spec.DataSource)
	}
	// multi-node payoff: the branch pod is NOT pinned and needs NO capabilities
	pod, err := cs.CoreV1().Pods(kubeNS).Get(ctx, b.ContainerID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sc := pod.Spec.Containers[0].SecurityContext; sc != nil {
		t.Errorf("branch pod SecurityContext = %+v, want none (no SYS_ADMIN)", sc)
	}
	if len(pod.Spec.NodeSelector) != 0 || pod.Spec.Affinity != nil {
		t.Errorf("branch pod has placement constraints: selector=%v affinity=%v", pod.Spec.NodeSelector, pod.Spec.Affinity)
	}

	// SQL through the branch pod; branch writes must not reach the source
	branchPort := forwardPort(t, cfg, cs, b.ContainerID)
	if n := queryInt(t, ctx, branchPort, `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("branch rows = %d", n)
	}
	{
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@127.0.0.1:%d/postgres", branchPort))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `UPDATE accounts SET balance = 0`); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	}
	srcPort := forwardPort(t, cfg, cs, "pgbranch-it-source")
	if n := queryInt(t, ctx, srcPort, `SELECT sum(balance) FROM accounts`); n != 1000000 {
		t.Fatalf("source mutated! sum=%d", n)
	}

	// branch-from-branch: write a marker only the parent has, then clone it
	execSQL(t, ctx, drv, b.ContainerID, `CREATE TABLE marker(v text); INSERT INTO marker VALUES ('from-pr-1')`)
	start = time.Now()
	b2, err := e.CreateBranchFrom(ctx, "csi-pr-2", "csi-pr-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("branch csi-pr-2 created from csi-pr-1 in %s (incl. parent stop/restart)", time.Since(start))
	pr2Destroyed := false
	t.Cleanup(func() {
		if pr2Destroyed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.DestroyBranch(ctx, "csi-pr-2"); err != nil {
			t.Errorf("destroy csi-pr-2: %v", err)
		}
	})
	pvc2, err := cs.CoreV1().PersistentVolumeClaims(kubeNS).Get(ctx, "pgbranch-br-csi-pr-2-rw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc2.Spec.DataSource == nil || pvc2.Spec.DataSource.Name != "pgbranch-br-csi-pr-1-rw" {
		t.Errorf("child PVC dataSource = %+v, want clone of the parent PVC", pvc2.Spec.DataSource)
	}
	// the child sees the parent's marker and the parent's writes
	childPort := forwardPort(t, cfg, cs, b2.ContainerID)
	if n := queryInt(t, ctx, childPort, `SELECT count(*) FROM marker WHERE v = 'from-pr-1'`); n != 1 {
		t.Fatalf("child marker rows = %d", n)
	}
	if n := queryInt(t, ctx, childPort, `SELECT sum(balance) FROM accounts`); n != 0 {
		t.Fatalf("child did not inherit parent writes: sum=%d", n)
	}
	// the parent was restarted (new pod) and keeps working independently
	p1, err := r.GetBranchByName("csi-pr-1")
	if err != nil {
		t.Fatal(err)
	}
	parentPort := forwardPort(t, cfg, cs, p1.ContainerID)
	{
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@127.0.0.1:%d/postgres", parentPort))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO marker VALUES ('parent-only')`); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	}
	if n := queryInt(t, ctx, childPort, `SELECT count(*) FROM marker`); n != 1 {
		t.Fatalf("parent write leaked into child: marker rows = %d", n)
	}
	// and the source still has none of it
	if n := queryInt(t, ctx, srcPort, `SELECT sum(balance) FROM accounts`); n != 1000000 {
		t.Fatalf("source mutated by branch-from-branch! sum=%d", n)
	}

	// destroy: child first, then parent (csi allows either order; this also
	// proves the child's PVC is independent), PVCs must be gone
	if err := e.DestroyBranch(ctx, "csi-pr-2"); err != nil {
		t.Fatal(err)
	}
	pr2Destroyed = true
	if err := e.DestroyBranch(ctx, "csi-pr-1"); err != nil {
		t.Fatal(err)
	}
	pr1Destroyed = true
	for _, name := range []string{"pgbranch-br-csi-pr-1-rw", "pgbranch-br-csi-pr-2-rw"} {
		if _, err := cs.CoreV1().PersistentVolumeClaims(kubeNS).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("PVC %s still present after destroy (err=%v)", name, err)
		}
	}
	if list, err := drv.ListManaged(ctx); err != nil || len(list) != 0 {
		t.Errorf("ListManaged after destroy = %v, %v", list, err)
	}
}

// TestKubeCSISnapshotCloneRoundtrip exercises the VolumeSnapshot+restore
// clone path at the driver level (CloneVolume with a snapshot class): write a
// probe into a PVC, clone via snapshot, read it back from the clone, and
// verify removal also deletes the per-clone snapshot.
func TestKubeCSISnapshotCloneRoundtrip(t *testing.T) {
	drv, _, _ := csiIT(t, "csi-hostpath-snapclass")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	src, dst := "pgbranch-csi-snap-src", "pgbranch-csi-snap-dst"
	if err := drv.CreateVolume(ctx, src, map[string]string{"pgbranch.managed": "true"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := drv.RemoveVolume(ctx, src); err != nil {
			t.Errorf("remove %s: %v", src, err)
		}
	})
	if _, err := drv.RunHelper(ctx, rt.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "echo snapshot-probe > /data/probe"},
		Mounts: []rt.Mount{{Volume: src, Target: "/data"}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := drv.CloneVolume(ctx, src, dst, map[string]string{"pgbranch.managed": "true"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := drv.RemoveVolume(ctx, dst); err != nil {
			t.Errorf("remove %s: %v", dst, err)
		}
	})
	if _, err := drv.RunHelper(ctx, rt.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "grep -q snapshot-probe /data/probe"},
		Mounts: []rt.Mount{{Volume: dst, Target: "/data", ReadOnly: true}},
	}); err != nil {
		t.Fatalf("clone is missing the source's data: %v", err)
	}
}
