// Helm deployment integration test against the pgoverlay-test kind cluster
// (hack/kind-up.sh), gated by PGOVERLAY_K8S_IT=1. Shell-driven on purpose: it
// exercises exactly what an operator runs (docker build, kind load, helm
// install, kubectl port-forward) and then drives branchd's REST API with the
// typed client. The chart is installed into a throwaway namespace and fully
// torn down at the end.
package deploy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
)

const (
	kindCluster = "pgoverlay-test"
	storageNode = "pgoverlay-test-control-plane"
	helmNS      = "pgoverlay-system"
	release     = "pgoverlay" // fullname collapses to "pgoverlay" -> svc pgoverlay-api
	apiToken    = "helm-it-token"
	sourcePod   = "pgoverlay-it-helm-source"
	// chartPath is spelled once: both suites install from it and chartImage
	// renders it to learn which image to build.
	chartPath = "deploy/helm/pgoverlay"
)

// run executes a command from the repo root and fails the test on error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// dumpOnFailure prints pod state, events and branchd logs for ns when the test
// has failed. `helm install --wait` reports only "Deployment not ready,
// Available: 0/1", which names the symptom and nothing else — without this the
// only way to find out why a pod never went Ready is to reproduce a kind
// cluster locally.
//
// Register it AFTER the uninstall cleanup: t.Cleanup runs LIFO, so registering
// it later makes it run earlier, and the evidence still exists when it does.
func dumpOnFailure(t *testing.T, kc, ns string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, args := range [][]string{
			{"get", "pods", "-o", "wide"},
			{"get", "events", "--sort-by=.lastTimestamp"},
			{"describe", "pods"},
			{"logs", "-l", "app.kubernetes.io/name=pgoverlay", "--all-containers",
				"--prefix", "--tail=100"},
		} {
			out, err := exec.Command("kubectl",
				append([]string{"--kubeconfig", kc, "-n", ns}, args...)...).CombinedOutput()
			t.Logf("=== kubectl -n %s %s (err=%v) ===\n%s",
				ns, strings.Join(args, " "), err, out)
		}
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeKubeconfig exports the kind cluster's kubeconfig to a temp file so
// every kubectl/helm call below is pinned to the test cluster.
func writeKubeconfig(t *testing.T) string {
	t.Helper()
	kc := run(t, "kind", "get", "kubeconfig", "--name", kindCluster)
	p := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(p, []byte(kc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// chartImage renders the chart and returns the branchd image reference it
// actually asks for.
//
// The name is NOT spelled here on purpose. Both suites install with
// image.pullPolicy=Never, so a pod can only ever start from an image already
// side-loaded into the kind node. If the name this test builds and the name
// the chart deploys ever drift apart — a rename that touches values.yaml but
// not the test, or the Makefile but not the chart — every pod strands in
// ErrImageNeverPull and the only symptom is "Available: 0/1" three minutes
// later. Deriving the string from the chart makes that class of drift
// impossible rather than merely unlikely.
func chartImage(t *testing.T) string {
	t.Helper()
	out := run(t, "helm", "template", release, chartPath,
		"--set", "node="+storageNode, "--set", "token=x")
	m := regexp.MustCompile(`(?m)^\s*image:\s*"?([^"\s]+)"?\s*$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no container image in the rendered chart %s:\n%s", chartPath, out)
	}
	return m[1]
}

// loadBranchdImage builds the image the chart asks for (see chartImage) and
// side-loads it into kind.
// `kind load docker-image` fails with Colima's containerd image store on
// multi-arch manifests, so export a single-platform archive (same trick as
// hack/kind-up.sh) and fall back to a plain save for older docker.
func loadBranchdImage(t *testing.T) {
	t.Helper()
	img := chartImage(t)
	t.Logf("chart asks for image %s; building and side-loading that", img)
	run(t, "docker", "build", "-t", img, ".")
	arch := strings.TrimSpace(run(t, "docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}"))
	tar := filepath.Join(t.TempDir(), "branchd.tar")
	if cmd := exec.Command("docker", "save", "--platform", arch, img, "-o", tar); cmd.Run() != nil {
		run(t, "docker", "save", img, "-o", tar)
	}
	run(t, "kind", "load", "image-archive", tar, "--name", kindCluster)
}

// portForward starts kubectl port-forward to target ("svc/pgoverlay-api", or
// "pod/<name>") on a random local port and returns the base URL once the
// forward is listening.
//
// ns and target are both explicit. ns because the HA suite installs its own
// release into its own namespace, and a helper that assumed helmNS forwarded
// to a Service that wasn't there. target because a port-forward is not a load
// balancer: it binds ONE pod for the life of the connection and dies with it,
// so a test that kills a pod has to say which pod it wants next.
func portForward(t *testing.T, kc, ns, target string) string {
	t.Helper()
	cmd := exec.Command("kubectl", "--kubeconfig", kc, "-n", ns,
		"port-forward", target, ":7070")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Keep a copy of stderr: when kubectl exits immediately (no such Service,
	// no such namespace) stdout closes empty, and without this the only
	// symptom is an empty line and no reason.
	var errOut syncBuffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &errOut)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case line := <-lines:
		m := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("kubectl port-forward -n %s %s gave no local address (stdout %q); stderr: %s",
				ns, target, line, strings.TrimSpace(errOut.String()))
		}
		return "http://127.0.0.1:" + m[1]
	case <-time.After(30 * time.Second):
		t.Fatalf("kubectl port-forward -n %s %s never became ready; stderr: %s",
			ns, target, strings.TrimSpace(errOut.String()))
		return ""
	}
}

// syncBuffer is a bytes.Buffer safe for the exec copy goroutine to write to
// while the test goroutine reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// readyProbe gates on the FINAL postgres server, not merely on "something is
// answering". See waitPostgresReady for why the TCP half is load-bearing; the
// socket half then proves the exact path every caller below uses is live.
const readyProbe = `pg_isready -U postgres -h 127.0.0.1 -p 5432 && ` +
	`psql -U postgres -h /var/run/postgresql -tAc 'SELECT 1'`

// waitPostgresReady blocks until pod is running the postgres server the test
// will actually talk to, and shares one implementation with the HA suite
// because the two copies of this wait had already drifted apart.
//
// The gate must not be a bare socket pg_isready. Initialising an empty PGDATA,
// the official image's entrypoint starts a TEMPORARY server to create the
// database and run /docker-entrypoint-initdb.d, then stops it (`pg_ctl -m fast
// -w stop`) before exec'ing the real one. That temporary server owns
// /var/run/postgresql/.s.PGSQL.5432 while it lives, so a socket pg_isready
// succeeds against it and returns — and the socket then vanishes for the
// restart. The next psql lands in that window and dies with
//
//	connection to server on socket "/var/run/postgresql/.s.PGSQL.5432" failed:
//	No such file or directory
//
// which is exactly how TestHelmDeployEndToEnd failed in CI 30312070784, in
// 20.75s: the wait returned early rather than exhausting its deadline. The
// race is not new, it was hidden — the helm install used to burn its full 3m
// timeout ahead of this call, which gave postgres all the time in the world.
//
// TCP discriminates because the entrypoint starts that temporary server under
// -c listen_addresses= (set to the empty string) on the command line, which
// overrides the image's own postgresql.conf (its Dockerfile rewrites the
// shipped sample to listen_addresses = '*'). The init server is therefore
// reachable ONLY over the socket and can never answer a TCP ping, so this
// loop cannot pass until the final server is up — no sleeping, no blind
// retry, no widened deadline.
// internal/runtime/kube_it_test.go gates the same image on the entrypoint's
// "PostgreSQL init process complete" log line; this asserts the same fact
// structurally instead of by matching a log string.
func waitPostgresReady(t *testing.T, kc, ns, pod string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, err := exec.Command("kubectl", "--kubeconfig", kc, "-n", ns,
			"exec", pod, "--", "sh", "-c", readyProbe).CombinedOutput()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres in %s/%s never accepted a query: %v\n%s", ns, pod, err, out)
		}
		time.Sleep(time.Second)
	}
}

// startSourcePod runs a vanilla "production" postgres pod for the engine to
// seed from (small local copy of the runtime IT helper, via kubectl):
// wal_level=replica plus a replication pg_hba entry appended after startup.
func startSourcePod(t *testing.T, kc string) (podIP string) {
	t.Helper()
	kubectl := func(args ...string) string {
		return run(t, "kubectl", append([]string{"--kubeconfig", kc, "-n", helmNS}, args...)...)
	}
	kubectl("run", sourcePod, "--image=postgres:17", "--restart=Never",
		"--env=POSTGRES_PASSWORD=secret", "--", "-c", "wal_level=replica", "-c", "max_wal_senders=4")
	t.Cleanup(func() {
		exec.Command("kubectl", "--kubeconfig", kc, "-n", helmNS,
			"delete", "pod", sourcePod, "--ignore-not-found", "--wait=false").Run()
	})
	waitPostgresReady(t, kc, helmNS, sourcePod)
	// stock pg_hba has no remote replication entry; branchd's pg_basebackup
	// helper would be rejected without this
	kubectl("exec", sourcePod, "--", "sh", "-c",
		`echo 'host replication all all scram-sha-256' >> "$PGDATA/pg_hba.conf"`)
	kubectl("exec", sourcePod, "--", "psql", "-U", "postgres", "-c", "SELECT pg_reload_conf()")
	kubectl("exec", sourcePod, "--", "psql", "-U", "postgres", "-c",
		"CREATE TABLE accounts(id int primary key); INSERT INTO accounts SELECT generate_series(1,1000)")
	ip := strings.TrimSpace(kubectl("get", "pod", sourcePod, "-o", "jsonpath={.status.podIP}"))
	if ip == "" {
		t.Fatal("source pod has no IP")
	}
	return ip
}

func TestHelmDeployEndToEnd(t *testing.T) {
	if os.Getenv("PGOVERLAY_K8S_IT") != "1" {
		t.Skip("set PGOVERLAY_K8S_IT=1 to run kubernetes integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	run(t, "hack/kind-up.sh")
	loadBranchdImage(t)
	kc := writeKubeconfig(t)

	// backstop teardown (the happy path uninstalls explicitly below)
	t.Cleanup(func() {
		exec.Command("helm", "--kubeconfig", kc, "uninstall", release, "-n", helmNS, "--wait").Run()
		exec.Command("kubectl", "--kubeconfig", kc, "delete", "namespace", helmNS,
			"--ignore-not-found", "--wait").Run()
	})
	dumpOnFailure(t, kc, helmNS)

	start := time.Now()
	run(t, "helm", "--kubeconfig", kc, "install", release, chartPath,
		"-n", helmNS, "--create-namespace",
		"--set", "node="+storageNode,
		"--set", "token="+apiToken,
		"--set", "image.pullPolicy=Never", // image was kind-loaded, never pull
		"--wait", "--timeout", "3m")
	t.Logf("helm release ready in %s", time.Since(start))

	base := portForward(t, kc, helmNS, "svc/pgoverlay-api")
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}

	srcIP := startSourcePod(t, kc)
	client := apiclient.New(base, apiToken)

	start = time.Now()
	src, err := client.CreateSource(ctx, api.CreateSourceRequest{
		Name: "helm-main", Host: srcIP, Port: 5432, User: "postgres", Password: "secret",
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if src.State != "ready" {
		t.Fatalf("source state = %q, want ready", src.State)
	}
	t.Logf("source seeded in %s", time.Since(start))

	start = time.Now()
	b, err := client.CreateBranch(ctx, api.CreateBranchRequest{Name: "helm-pr-1", Source: "helm-main"})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	t.Logf("branch helm-pr-1 created in %s (host %s:%d)", time.Since(start), b.Host, b.Port)
	got, err := client.GetBranch(ctx, "helm-pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "ready" {
		t.Errorf("branch state = %q, want ready", got.State)
	}
	if got.Host == "" || got.Host == "127.0.0.1" || got.Port != 5432 {
		t.Errorf("branch address = %s:%d, want pod IP:5432", got.Host, got.Port)
	}

	if err := client.DestroyBranch(ctx, "helm-pr-1"); err != nil {
		t.Fatalf("destroy branch: %v", err)
	}
	if err := client.RemoveSource(ctx, "helm-main"); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	if out := strings.TrimSpace(run(t, "kubectl", "--kubeconfig", kc, "-n", helmNS,
		"get", "pods", "-l", "pgoverlay.managed=true", "-o", "name")); out != "" {
		t.Errorf("leftover pgoverlay-managed pods after destroy: %s", out)
	}

	run(t, "kubectl", "--kubeconfig", kc, "-n", helmNS, "delete", "pod", sourcePod, "--wait")
	run(t, "helm", "--kubeconfig", kc, "uninstall", release, "-n", helmNS, "--wait")
	run(t, "kubectl", "--kubeconfig", kc, "delete", "namespace", helmNS, "--wait")
	fmt.Println("helm e2e: install, source, branch, destroy, uninstall — all clean")
}
