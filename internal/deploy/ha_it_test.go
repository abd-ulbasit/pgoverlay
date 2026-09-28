// HA leader-election integration test against the pgoverlay-test kind cluster
// (hack/kind-up.sh), gated by PGOVERLAY_K8S_IT=1. It installs the chart with
// replicaCount=2 (which turns on --leader-elect + the leases RBAC), asserts
// exactly one replica holds the pgoverlay-branchd Lease, carries the
// pgoverlay.leader label and is the only endpoint of the API Service, and that
// mutations through the Service succeed. It then kills the leader pod and
// asserts the surviving replica acquires the Lease, takes over the label and
// the Service, and a branch create through the Service succeeds.
//
// NOT RUN in this change's sandbox (no kind/Docker) and NOT in default CI
// (CI runs PGOVERLAY_IT only, not PGOVERLAY_K8S_IT). Written to compile and be
// correct; reuses the helm/port-forward/source-pod helpers in helm_it_test.go.
package deploy

import (
	"context"
	"os"
	"os/exec"
	"strconv"
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

	// The leader labels its pod and the API Service selects only that label,
	// so the Service (what ghook, the CLI and `port-forward svc/...` use) has
	// exactly the leader as its endpoint and mutations through it never land
	// on a follower. Both pods stay Ready (helm --wait above needed that).
	waitServiceIsLeaderOnly(t, kc, holder)
	base := portForward(t, kc, haNS, "svc/"+haAPIService)
	client := apiclient.New(base, haToken)
	srcIP := startHASourcePod(t, kc)

	if _, err := createSourceWithRetry(ctx, client, srcIP, renewBound); err != nil {
		t.Fatalf("create source through the API Service: %v", err)
	}
	// Every mutation through the Service lands on the leader: no 503s.
	for i := 1; i <= 4; i++ {
		name := "ha-pr-" + strconv.Itoa(i)
		if _, err := client.CreateBranch(ctx, api.CreateBranchRequest{Name: name, Source: "ha-main"}); err != nil {
			t.Fatalf("create %s through the API Service: %v", name, err)
		}
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

	// The new leader takes over the label, so the Service follows it. The old
	// forward went down with the pod we just deleted (a Service port-forward
	// pins one pod), so re-establish it before asserting writes.
	waitServiceIsLeaderOnly(t, kc, newHolder)
	client = apiclient.New(portForward(t, kc, haNS, "svc/"+haAPIService), haToken)

	// A create now succeeds against the new leader within the budget.
	if _, err := createBranchWithRetry(ctx, client, "ha-pr-5", renewBound); err != nil {
		t.Fatalf("create branch after failover: %v", err)
	}

	// Cleanup branches/source so the namespace teardown is clean.
	for i := 1; i <= 5; i++ {
		_ = client.DestroyBranch(ctx, "ha-pr-"+strconv.Itoa(i))
	}
	_ = client.RemoveSource(ctx, "ha-main")
}

// haAPIService is the chart's API Service for release haRelease (the release
// name contains "pgoverlay", so the fullname is the release name).
const haAPIService = haRelease + "-api"

// waitServiceIsLeaderOnly polls until leader carries the pgoverlay.leader
// label, no other branchd pod does, and the API Service's only endpoint is
// leader.
func waitServiceIsLeaderOnly(t *testing.T, kc, leader string) {
	t.Helper()
	kubectl := func(args ...string) (string, error) {
		out, err := exec.Command("kubectl", append([]string{"--kubeconfig", kc, "-n", haNS}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	var labelled, endpoints string
	deadline := time.Now().Add(renewBound)
	for time.Now().Before(deadline) {
		var err1, err2 error
		labelled, err1 = kubectl("get", "pods", "-l", "app.kubernetes.io/name=pgoverlay,pgoverlay.leader=true",
			"-o", "jsonpath={.items[*].metadata.name}")
		endpoints, err2 = kubectl("get", "endpointslices", "-l", "kubernetes.io/service-name="+haAPIService,
			"-o", "jsonpath={.items[*].endpoints[*].targetRef.name}")
		if err1 == nil && err2 == nil && labelled == leader && endpoints == leader {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("API Service never routed to the leader %s alone within %s: labelled pods %q, endpoints %q",
		leader, renewBound, labelled, endpoints)
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
