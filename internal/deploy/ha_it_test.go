// HA leader-election integration test against the pgbranch-test kind cluster
// (hack/kind-up.sh), gated by PGBRANCH_K8S_IT=1. It installs the chart with
// replicaCount=2 (which turns on --leader-elect + the leases RBAC), asserts
// exactly one replica holds the pgbranch-branchd Lease and serves mutations,
// kills the leader pod, and asserts the surviving replica acquires the Lease
// and a branch create succeeds within the renew deadline.
//
// NOT RUN in this change's sandbox (no kind/Docker) and NOT in default CI
// (CI runs PGBRANCH_IT only, not PGBRANCH_K8S_IT). Written to compile and be
// correct; reuses the helm/port-forward/source-pod helpers in helm_it_test.go.
package deploy

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/apiclient"
)

const (
	haNS      = "pgbranch-ha"
	haRelease = "pgbranch"
	haToken   = "ha-it-token"
	haSrcPod  = "pgbranch-ha-source"
	leaseName = "pgbranch-branchd"
	// failover budget: the lease duration is 15s, so a survivor can acquire
	// within ~15s of the old holder going away; allow generous slack for the
	// re-acquire and the subsequent mutating create through the Service.
	renewBound = 60 * time.Second
)

// leaseHolder returns the holderIdentity of the pgbranch-branchd Lease ("" if
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
	if os.Getenv("PGBRANCH_K8S_IT") != "1" {
		t.Skip("set PGBRANCH_K8S_IT=1 to run kubernetes integration tests")
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

	// 2 replicas → the chart renders --leader-elect, POD_NAME and the leases
	// RBAC. Both pods co-schedule to the storage node (RWO state dir).
	run(t, "helm", "--kubeconfig", kc, "install", haRelease, "deploy/helm/pgbranch",
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
		"-l", "app.kubernetes.io/name=pgbranch", "-o", "jsonpath={.items[*].metadata.name}")))
	if len(pods) != 2 {
		t.Fatalf("want 2 branchd pods, got %d: %v", len(pods), pods)
	}
	if holder != pods[0] && holder != pods[1] {
		t.Fatalf("Lease holder %q is not one of the branchd pods %v", holder, pods)
	}

	// The leader serves mutations; seed a source + create a branch through the
	// Service (which load-balances to whichever replica — non-leaders 503 on
	// mutations, but the Service has only the leader ready for writes... so we
	// drive the API and tolerate a transient 503 by retrying through the LB).
	base := portForward(t, kc)
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
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := exec.Command("kubectl", "--kubeconfig", kc, "-n", haNS, "exec", haSrcPod,
			"--", "pg_isready", "-U", "postgres", "-h", "/var/run/postgresql").Run()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HA source pod never became ready: %v", err)
		}
		time.Sleep(time.Second)
	}
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
