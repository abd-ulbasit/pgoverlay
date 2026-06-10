// Kubernetes integration tests against the pgbranch-test kind cluster
// (hack/kind-up.sh). Gated by PGBRANCH_K8S_IT=1. External test package:
// the e2e test drives the engine, which imports runtime.
package runtime_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	rt "github.com/abd-ulbasit/pgbranch/internal/runtime"
)

const (
	kindCluster  = "pgbranch-test"
	kubeNS       = "default"
	kindNodeName = "pgbranch-test-control-plane" // single node = storage node
	kubeDataRoot = "/var/lib/pgbranch"           // inside the kind node container (under its /var volume, so overlay upperdirs work)
)

// kubeIT ensures the kind cluster exists and returns the driver plus raw
// client-go handles (for the source pod and port-forwarding).
func kubeIT(t *testing.T) (*rt.KubeDriver, *kubernetes.Clientset, *rest.Config) {
	t.Helper()
	if os.Getenv("PGBRANCH_K8S_IT") != "1" {
		t.Skip("set PGBRANCH_K8S_IT=1 to run kubernetes integration tests")
	}
	if out, err := exec.Command("../../hack/kind-up.sh").CombinedOutput(); err != nil {
		t.Fatalf("hack/kind-up.sh: %v\n%s", err, out)
	}
	kc, err := exec.Command("kind", "get", "kubeconfig", "--name", kindCluster).Output()
	if err != nil {
		t.Fatalf("kind get kubeconfig: %v", err)
	}
	kcPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kcPath, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	drv, err := rt.NewKubeDriver(kcPath, kubeNS, kindNodeName, kubeDataRoot)
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

func TestKubeVolumeAndHelperRoundtrip(t *testing.T) {
	drv, _, _ := kubeIT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	vol := "pgbranch-test-vol"
	if err := drv.CreateVolume(ctx, vol, map[string]string{"pgbranch.managed": "true"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := drv.RemoveVolume(ctx, vol); err != nil {
			t.Errorf("remove volume: %v", err)
		}
	})

	// write via one helper, verify (and check the labels file) via another
	if err := drv.RunHelper(ctx, rt.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "echo hello > /data/probe"},
		Mounts: []rt.Mount{{Volume: vol, Target: "/data"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := drv.RunHelper(ctx, rt.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "grep -q hello /data/probe && grep -q pgbranch.managed /data/.pgbranch-labels.json"},
		Mounts: []rt.Mount{{Volume: vol, Target: "/data", ReadOnly: true}},
	}); err != nil {
		t.Fatal(err)
	}
	// failing helper surfaces its logs in the error
	err := drv.RunHelper(ctx, rt.HelperSpec{Image: "alpine:3.21", Cmd: []string{"sh", "-c", "echo boom >&2; exit 3"}})
	if err == nil {
		t.Fatal("want error from non-zero helper exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("helper error %q does not include pod logs", err)
	}
}

// startSourcePod runs a vanilla "production" postgres pod the engine will
// seed from (the kube equivalent of pgctl.StartSourcePG): wal_level=replica,
// replication pg_hba entry appended + reloaded after startup.
func startSourcePod(t *testing.T, ctx context.Context, drv *rt.KubeDriver, cs *kubernetes.Clientset) (podIP string) {
	t.Helper()
	const name = "pgbranch-it-source"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: kubeNS},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "postgres",
				Image: "postgres:17",
				Env:   []corev1.EnvVar{{Name: "POSTGRES_PASSWORD", Value: "secret"}},
				Args:  []string{"-c", "wal_level=replica", "-c", "max_wal_senders=4"},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(kubeNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := drv.StopRemove(ctx, name); err != nil {
			t.Errorf("remove source pod: %v", err)
		}
	})
	waitExecOK(t, ctx, drv, name, 2*time.Minute, []string{"pg_isready", "-U", "postgres", "-h", "/var/run/postgresql"})
	// stock pg_hba has no remote replication entry; pg_basebackup from the
	// helper pod would be rejected without this
	if err := drv.Exec(ctx, name, []string{"sh", "-c", `echo 'host replication all all scram-sha-256' >> "$PGDATA/pg_hba.conf"`}); err != nil {
		t.Fatal(err)
	}
	if err := drv.Exec(ctx, name, []string{"psql", "-U", "postgres", "-c", "SELECT pg_reload_conf()"}); err != nil {
		t.Fatal(err)
	}
	info, err := drv.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Host == "" {
		t.Fatal("source pod has no IP")
	}
	return info.Host
}

func waitExecOK(t *testing.T, ctx context.Context, drv *rt.KubeDriver, pod string, timeout time.Duration, cmd []string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = drv.Exec(ctx, pod, cmd); last == nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("pod %s never became ready: %v", pod, last)
}

// forwardPort port-forwards local:5432 to the pod via client-go SPDY and
// returns the local port (how the host-side test reaches pod IPs).
func forwardPort(t *testing.T, cfg *rest.Config, cs *kubernetes.Clientset, pod string) int {
	t.Helper()
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	url := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(kubeNS).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", url)
	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	fw, err := portforward.New(dialer, []string{"0:5432"}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := fw.ForwardPorts(); err != nil {
			t.Logf("port-forward %s: %v", pod, err)
		}
	}()
	t.Cleanup(func() { close(stopCh) })
	select {
	case <-readyCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("port-forward to %s not ready", pod)
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("forwarded ports: %v %v", ports, err)
	}
	return int(ports[0].Local)
}

func queryInt(t *testing.T, ctx context.Context, port int, q string) int {
	t.Helper()
	c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@127.0.0.1:%d/postgres", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestKubeEndToEndBranching(t *testing.T) {
	drv, cs, cfg := kubeIT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	srcIP := startSourcePod(t, ctx, drv, cs)
	if err := drv.Exec(ctx, "pgbranch-it-source", []string{"psql", "-U", "postgres", "-c",
		`CREATE TABLE accounts(id int primary key, balance int);
		 INSERT INTO accounts SELECT i, 100 FROM generate_series(1,10000) i`}); err != nil {
		t.Fatal(err)
	}

	r, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() }) // after the destroy cleanups below (LIFO)
	e := engine.New(r, drv, "postgres:17")

	src := &registry.Source{Name: "k8s-main", PGVersion: "17", ConnHost: srcIP, ConnPort: 5432, ConnUser: "postgres"}
	start := time.Now()
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Logf("source seeded in %s", time.Since(start))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := e.RemoveSource(ctx, "k8s-main"); err != nil {
			t.Errorf("remove source: %v", err)
		}
	})

	start = time.Now()
	b, err := e.CreateBranch(ctx, "k8s-pr-1", "k8s-main", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("branch k8s-pr-1 created in %s (pod %s, host %s:%d)", time.Since(start), b.ContainerID, b.Host, b.Port)
	destroyed := false
	t.Cleanup(func() {
		if destroyed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := e.DestroyBranch(ctx, "k8s-pr-1"); err != nil {
			t.Errorf("destroy k8s-pr-1: %v", err)
		}
	})
	if b.ContainerID != "pgbranch-br-k8s-pr-1" {
		t.Errorf("branch pod name = %q", b.ContainerID)
	}
	if b.Host == "" || b.Host == "127.0.0.1" || b.Port != 5432 {
		t.Errorf("branch address = %s:%d, want pod IP:5432", b.Host, b.Port)
	}

	// SQL through the branch pod (port-forward stands in for in-cluster access)
	branchPort := forwardPort(t, cfg, cs, b.ContainerID)
	if n := queryInt(t, ctx, branchPort, `SELECT count(*) FROM accounts`); n != 10000 {
		t.Fatalf("branch rows = %d", n)
	}
	// writes to the branch must not reach the source
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

	if list, err := drv.ListManaged(ctx); err != nil || len(list) != 1 {
		t.Fatalf("ListManaged = %v, %v (want the one branch pod)", list, err)
	}

	start = time.Now()
	if err := e.DestroyBranch(ctx, "k8s-pr-1"); err != nil {
		t.Fatal(err)
	}
	destroyed = true
	t.Logf("branch destroyed in %s", time.Since(start))
	if _, err := cs.CoreV1().Pods(kubeNS).Get(ctx, b.ContainerID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("branch pod still present after destroy (err=%v)", err)
	}
	if list, err := drv.ListManaged(ctx); err != nil || len(list) != 0 {
		t.Errorf("ListManaged after destroy = %v, %v", list, err)
	}
}
