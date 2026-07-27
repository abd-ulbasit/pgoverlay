// HA leader-election integration test against the pgoverlay-test kind cluster
// (hack/kind-up.sh), gated by PGOVERLAY_K8S_IT=1. It installs the chart with
// replicaCount=2 (which turns on --leader-elect + the leases RBAC), asserts
// exactly one replica holds the pgoverlay-branchd Lease and serves mutations,
// kills the leader pod, and asserts the surviving replica acquires the Lease
// and a branch create succeeds within the renew deadline.
//
// NOT RUN in this change's sandbox (no kind/Docker) and NOT in default CI
// (CI runs PGOVERLAY_IT only, not PGOVERLAY_K8S_IT). Written to compile and be
// correct; reuses the helm/port-forward/source-pod helpers in helm_it_test.go.
package deploy

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
)

const (
	haNS      = "pgoverlay-ha"
	haRelease = "pgoverlay"
	haToken   = "ha-it-token"
	haSrcPod  = "pgoverlay-ha-source"
	leaseName = "pgoverlay-branchd"
	// failover budget: the lease duration is 15s, so a survivor can acquire
	// within ~15s of the old holder going away; allow generous slack for the
	// re-acquire and the subsequent mutating create through the Service.
	renewBound = 60 * time.Second
)

// leaseHolder returns the holderIdentity of the pgoverlay-branchd Lease ("" if
// the Lease does not exist yet).
func leaseHolder(t *testing.T, kc string) string {
	t.Helper()
	out, err := exec.Command("kubectl", "--kubeconfig", kc, "-n", haNS,
		"get", "lease", leaseName, "-o", "jsonpath={.spec.holderIdentity}").CombinedOutput()
	if err != nil {
		return "" // not created yet
	}
	return strings.TrimSpace(string(out))
}

// waitLeaseHolder polls until the Lease has a holder, returning it.
func waitLeaseHolder(t *testing.T, kc string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h := leaseHolder(t, kc); h != "" {
			return h
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Lease %s never acquired a holder within %s", leaseName, timeout)
	return ""
}

func TestHelmLeaderElectionFailover(t *testing.T) {
	if os.Getenv("PGOVERLAY_K8S_IT") != "1" {
		t.Skip("set PGOVERLAY_K8S_IT=1 to run kubernetes integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	run(t, "hack/kind-up.sh")
	loadBranchdImage(t)
	kc := writeKubeconfig(t)

	kubectl := func(args ...string) string {
		return run(t, "kubectl", append([]string{"--kubeconfig", kc, "-n", haNS}, args...)...)
	}

	t.Cleanup(func() {
		exec.Command("helm", "--kubeconfig", kc, "uninstall", haRelease, "-n", haNS, "--wait").Run()
		exec.Command("kubectl", "--kubeconfig", kc, "delete", "namespace", haNS,
			"--ignore-not-found", "--wait").Run()
	})
	dumpOnFailure(t, kc, haNS)

	// 2 replicas → the chart renders --leader-elect, POD_NAME and the leases
	// RBAC. Both pods co-schedule to the storage node (RWO state dir).
	run(t, "helm", "--kubeconfig", kc, "install", haRelease, chartPath,
		"-n", haNS, "--create-namespace",
		"--set", "node="+storageNode,
		"--set", "token="+haToken,
		"--set", "replicaCount=2",
		"--set", "image.pullPolicy=Never",
		"--wait", "--timeout", "3m")

	// Exactly one replica holds the Lease.
	holder := waitLeaseHolder(t, kc, time.Minute)
	t.Logf("initial Lease holder: %s", holder)
	pods := strings.Fields(strings.TrimSpace(kubectl("get", "pods",
		"-l", "app.kubernetes.io/name=pgoverlay", "-o", "jsonpath={.items[*].metadata.name}")))
	if len(pods) != 2 {
		t.Fatalf("want 2 branchd pods, got %d: %v", len(pods), pods)
	}
	if holder != pods[0] && holder != pods[1] {
		t.Fatalf("Lease holder %q is not one of the branchd pods %v", holder, pods)
	}

	// Forward to the leader POD, not to the Service. /readyz is deliberately
	// NOT leader-gated (a follower stays ready to serve reads and probes), so
	// both replicas are Service endpoints and `port-forward svc/...` would pin
	// one at random — half the time a follower, which 503s every mutation for
	// as long as the forward lives. Naming the pod makes the test deterministic
	// and is what lets the post-failover step below prove the *new* leader
	// accepts writes.
	base := portForward(t, kc, haNS, "pod/"+holder)
	client := apiclient.New(base, haToken)
	srcIP := startHASourcePod(t, kc)

	if _, err := createSourceWithRetry(ctx, client, srcIP, renewBound); err != nil {
		t.Fatalf("create source against leader: %v", err)
	}
	if _, err := createBranchWithRetry(ctx, client, "ha-pr-1", renewBound); err != nil {
		t.Fatalf("create branch against leader: %v", err)
	}

	// Kill the leader pod; the survivor must acquire the Lease and accept a
	// mutating create within the renew deadline.
	kubectl("delete", "pod", holder, "--wait=false")
	deadline := time.Now().Add(renewBound)
	var newHolder string
	for time.Now().Before(deadline) {
		if h := leaseHolder(t, kc); h != "" && h != holder {
			newHolder = h
			break
		}
		time.Sleep(time.Second)
	}
	if newHolder == "" {
		t.Fatalf("Lease was not re-acquired by a surviving replica within %s", renewBound)
	}
	t.Logf("failed over: new Lease holder %s", newHolder)

	// The old forward went down with the pod we just deleted ("lost connection
	// to pod"), so every later request would get connection-refused on a dead
	// local port and look like a failover failure. Re-establish against the
	// survivor before asserting it accepts writes.
	client = apiclient.New(portForward(t, kc, haNS, "pod/"+newHolder), haToken)

	// A create now succeeds against the new leader within the budget.
	if _, err := createBranchWithRetry(ctx, client, "ha-pr-2", renewBound); err != nil {
		t.Fatalf("create branch after failover: %v", err)
	}

	// Cleanup branches/source so the namespace teardown is clean.
	_ = client.DestroyBranch(ctx, "ha-pr-1")
	_ = client.DestroyBranch(ctx, "ha-pr-2")
	_ = client.RemoveSource(ctx, "ha-main")
}

// startHASourcePod runs the seed postgres pod in the HA namespace.
func startHASourcePod(t *testing.T, kc string) string {
	t.Helper()
	kubectl := func(args ...string) string {
		return run(t, "kubectl", append([]string{"--kubeconfig", kc, "-n", haNS}, args...)...)
	}
	kubectl("run", haSrcPod, "--image=postgres:17", "--restart=Never",
		"--env=POSTGRES_PASSWORD=secret", "--", "-c", "wal_level=replica", "-c", "max_wal_senders=4")
	t.Cleanup(func() {
		exec.Command("kubectl", "--kubeconfig", kc, "-n", haNS,
			"delete", "pod", haSrcPod, "--ignore-not-found", "--wait=false").Run()
	})
	waitPostgresReady(t, kc, haNS, haSrcPod)
	kubectl("exec", haSrcPod, "--", "sh", "-c",
		`echo 'host replication all all scram-sha-256' >> "$PGDATA/pg_hba.conf"`)
	kubectl("exec", haSrcPod, "--", "psql", "-U", "postgres", "-c", "SELECT pg_reload_conf()")
	ip := strings.TrimSpace(kubectl("get", "pod", haSrcPod, "-o", "jsonpath={.status.podIP}"))
	if ip == "" {
		t.Fatal("HA source pod has no IP")
	}
	return ip
}

// createSourceWithRetry calls CreateSource, retrying on the transient 503 a
// non-leader replica returns while the Service still routes to it.
func createSourceWithRetry(ctx context.Context, c *apiclient.Client, srcIP string, within time.Duration) (*api.Source, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		src, err := c.CreateSource(ctx, api.CreateSourceRequest{
			Name: "ha-main", Host: srcIP, Port: 5432, User: "postgres", Password: "secret",
		})
		if err == nil {
			return src, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, lastErr
}

// createBranchWithRetry calls CreateBranch, retrying on the transient 503 a
// non-leader returns (the Service may route to a follower mid-failover).
func createBranchWithRetry(ctx context.Context, c *apiclient.Client, name string, within time.Duration) (*api.Branch, error) {
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		b, err := c.CreateBranch(ctx, api.CreateBranchRequest{Name: name, Source: "ha-main"})
		if err == nil {
			return b, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, lastErr
}
