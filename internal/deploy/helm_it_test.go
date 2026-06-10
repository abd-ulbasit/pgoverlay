// Helm deployment integration test against the pgbranch-test kind cluster
// (hack/kind-up.sh), gated by PGBRANCH_K8S_IT=1. Shell-driven on purpose: it
// exercises exactly what an operator runs (docker build, kind load, helm
// install, kubectl port-forward) and then drives branchd's REST API with the
// typed client. The chart is installed into a throwaway namespace and fully
// torn down at the end.
package deploy

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/apiclient"
)

const (
	kindCluster = "pgbranch-test"
	storageNode = "pgbranch-test-control-plane"
	helmNS      = "pgbranch-system"
	release     = "pgbranch" // fullname collapses to "pgbranch" -> svc pgbranch-api
	apiToken    = "helm-it-token"
	sourcePod   = "pgbranch-it-helm-source"
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

// loadBranchdImage builds pgbranch/branchd:dev and side-loads it into kind.
// `kind load docker-image` fails with Colima's containerd image store on
// multi-arch manifests, so export a single-platform archive (same trick as
// hack/kind-up.sh) and fall back to a plain save for older docker.
func loadBranchdImage(t *testing.T) {
	t.Helper()
	run(t, "docker", "build", "-t", "pgbranch/branchd:dev", ".")
	arch := strings.TrimSpace(run(t, "docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}"))
	tar := filepath.Join(t.TempDir(), "branchd.tar")
	if cmd := exec.Command("docker", "save", "--platform", arch, "pgbranch/branchd:dev", "-o", tar); cmd.Run() != nil {
		run(t, "docker", "save", "pgbranch/branchd:dev", "-o", tar)
	}
	run(t, "kind", "load", "image-archive", tar, "--name", kindCluster)
}

// portForward starts kubectl port-forward to the api Service on a random
// local port and returns the base URL once the forward is listening.
func portForward(t *testing.T, kc string) string {
	t.Helper()
	cmd := exec.Command("kubectl", "--kubeconfig", kc, "-n", helmNS,
		"port-forward", "svc/pgbranch-api", ":7070")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
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
			t.Fatalf("unexpected port-forward output: %q", line)
		}
		return "http://127.0.0.1:" + m[1]
	case <-time.After(30 * time.Second):
		t.Fatal("kubectl port-forward never became ready")
		return ""
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
	// readiness: pg_isready over the unix socket (TCP comes up before the
	// init scripts finish, the socket only after)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := exec.Command("kubectl", "--kubeconfig", kc, "-n", helmNS, "exec", sourcePod,
			"--", "pg_isready", "-U", "postgres", "-h", "/var/run/postgresql").Run()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("source pod never became ready: %v", err)
		}
		time.Sleep(time.Second)
	}
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
	if os.Getenv("PGBRANCH_K8S_IT") != "1" {
		t.Skip("set PGBRANCH_K8S_IT=1 to run kubernetes integration tests")
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

	start := time.Now()
	run(t, "helm", "--kubeconfig", kc, "install", release, "deploy/helm/pgbranch",
		"-n", helmNS, "--create-namespace",
		"--set", "node="+storageNode,
		"--set", "token="+apiToken,
		"--set", "image.pullPolicy=Never", // image was kind-loaded, never pull
		"--wait", "--timeout", "3m")
	t.Logf("helm release ready in %s", time.Since(start))

	base := portForward(t, kc)
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
		"get", "pods", "-l", "pgbranch.managed=true", "-o", "name")); out != "" {
		t.Errorf("leftover pgbranch-managed pods after destroy: %s", out)
	}

	run(t, "kubectl", "--kubeconfig", kc, "-n", helmNS, "delete", "pod", sourcePod, "--wait")
	run(t, "helm", "--kubeconfig", kc, "uninstall", release, "-n", helmNS, "--wait")
	run(t, "kubectl", "--kubeconfig", kc, "delete", "namespace", helmNS, "--wait")
	fmt.Println("helm e2e: install, source, branch, destroy, uninstall — all clean")
}
