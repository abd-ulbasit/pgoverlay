// Offline helm-template tests for the chart's env wiring (no cluster, no
// PGBRANCH_K8S_IT gate — `helm template` renders locally). Skipped when the
// helm binary isn't installed.
package deploy

import (
	"os/exec"
	"strings"
	"testing"
)

// helmTemplate renders the chart with the given --set values and returns the
// manifests plus the error (some tests expect rendering to fail).
func helmTemplate(t *testing.T, sets ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	args := []string{"template", "test-release", "deploy/helm/pgbranch",
		"--set", "node=storage-1", "--set", "token=t0k"}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// csiSets is the minimal valid csi storage configuration.
func csiSets(extra ...string) []string {
	return append([]string{
		"storage.mode=csi",
		"storage.storageClass=fast-clone",
	}, extra...)
}

// Registry-on-a-PVC: persistence.enabled is a tri-state string — "" (auto:
// ON with storage.mode=csi, OFF with hostpath), "true", "false". When on,
// branchd's state dir is a PVC mount instead of hostPath <dataRoot>/state.

func TestHelmPersistenceDefaultOffOnHostpath(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Error("hostpath default rendered a state PVC")
	}
	if !strings.Contains(out, "path: /var/lib/pgbranch/state") {
		t.Error("hostpath default lost the hostPath state volume")
	}
	if strings.Contains(out, "claimName:") {
		t.Error("hostpath default mounts a PVC claim")
	}
}

func TestHelmPersistenceAutoOnWithCSI(t *testing.T) {
	out, err := helmTemplate(t, csiSets()...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Error("csi mode did not auto-enable the state PVC")
	}
	if !strings.Contains(out, "claimName: test-release-pgbranch-state") {
		t.Errorf("deployment state volume is not the PVC claim:\n%s", out)
	}
	if strings.Contains(out, "path: /var/lib/pgbranch/state") {
		t.Error("csi mode still renders the hostPath state volume")
	}
	// default size
	if !strings.Contains(out, "storage: 1Gi") {
		t.Error("default persistence.size 1Gi not rendered")
	}
}

func TestHelmPersistenceExplicitTrueOnHostpath(t *testing.T) {
	out, err := helmTemplate(t, "persistence.enabled=true")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kind: PersistentVolumeClaim") || !strings.Contains(out, "claimName:") {
		t.Errorf("persistence.enabled=true on hostpath did not render the PVC:\n%s", out)
	}
	if strings.Contains(out, "path: /var/lib/pgbranch/state") {
		t.Error("explicit persistence still renders the hostPath state volume")
	}
}

func TestHelmPersistenceExplicitFalseOnCSI(t *testing.T) {
	out, err := helmTemplate(t, csiSets("persistence.enabled=false")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "kind: PersistentVolumeClaim") {
		t.Error("explicit persistence.enabled=false with csi must stay off")
	}
	if !strings.Contains(out, "path: /var/lib/pgbranch/state") {
		t.Error("disabled persistence lost the hostPath state volume")
	}
}

func TestHelmPersistenceSizeAndStorageClass(t *testing.T) {
	out, err := helmTemplate(t,
		"persistence.enabled=true", "persistence.size=5Gi", "persistence.storageClass=fast")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{"storage: 5Gi", "storageClassName: fast"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}
	// no storageClassName line when unset (cluster default class)
	out, err = helmTemplate(t, "persistence.enabled=true")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "storageClassName:") {
		t.Error("empty persistence.storageClass must omit storageClassName")
	}
}

func TestHelmRotateBranchCredentialsArg(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "--rotate-branch-credentials") {
		t.Error("rotation arg rendered by default (must be opt-in)")
	}
	out, err = helmTemplate(t, "rotateBranchCredentials=true")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--rotate-branch-credentials") {
		t.Error("rotateBranchCredentials=true did not render the branchd arg")
	}
}

// ghookSets is the minimal valid ghook configuration.
func ghookSets(extra ...string) []string {
	return append([]string{
		"ghook.enabled=true",
		"ghook.webhookSecret=wh-secret",
		"ghook.source=main",
	}, extra...)
}

func TestHelmGhookAppAuthEnvWiring(t *testing.T) {
	out, err := helmTemplate(t, ghookSets(
		"ghook.appId=12345",
		"ghook.appPrivateKey=fake-pem-key",
	)...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{"GHOOK_APP_ID", `"12345"`, "GHOOK_APP_PRIVATE_KEY", "app-private-key"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}
	if strings.Contains(out, "GHOOK_GITHUB_TOKEN") {
		t.Error("App mode must not wire GHOOK_GITHUB_TOKEN (App and PAT auth are mutually exclusive)")
	}
}

func TestHelmGhookPATEnvWiring(t *testing.T) {
	out, err := helmTemplate(t, ghookSets("ghook.githubToken=ghp_x")...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{"GHOOK_GITHUB_TOKEN", "github-token"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifests missing %q", want)
		}
	}
	if strings.Contains(out, "GHOOK_APP_ID") {
		t.Error("PAT mode must not wire GHOOK_APP_ID")
	}
}

func TestHelmGhookAppAndPATConflictFails(t *testing.T) {
	out, err := helmTemplate(t, ghookSets(
		"ghook.appId=12345",
		"ghook.appPrivateKey=fake-pem-key",
		"ghook.githubToken=ghp_x",
	)...)
	if err == nil {
		t.Fatalf("helm template succeeded with both App and PAT auth set:\n%s", out)
	}
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("error should name the conflict, got:\n%s", out)
	}
}

func TestHelmGhookAppIdWithoutKeyFails(t *testing.T) {
	out, err := helmTemplate(t, ghookSets("ghook.appId=12345")...)
	if err == nil {
		t.Fatalf("helm template succeeded with appId but no private key:\n%s", out)
	}
}

// Proxy wire-TLS: unset by default (the proxy answers SSLRequest with 'N'),
// and when proxy.tls.certSecret is set the secret is mounted and the
// --pg-tls-cert/--pg-tls-key args are wired to it.
func TestHelmProxyTLSDefaultOff(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if strings.Contains(out, "--pg-tls-cert") {
		t.Error("proxy TLS args rendered by default (must be opt-in)")
	}
	if strings.Contains(out, "name: proxy-tls") {
		t.Error("proxy-tls volume rendered by default")
	}
}

func TestHelmProxyTLSWiredWhenCertSecretSet(t *testing.T) {
	out, err := helmTemplate(t, "proxy.tls.certSecret=branchd-proxy-tls")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{
		"--pg-tls-cert=/etc/pgbranch/proxy-tls/tls.crt",
		"--pg-tls-key=/etc/pgbranch/proxy-tls/tls.key",
		"mountPath: /etc/pgbranch/proxy-tls",
		"secretName: branchd-proxy-tls",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("proxy TLS wiring missing %q:\n%s", want, out)
		}
	}
}

// The deployer RBAC is a namespaced Role (not a ClusterRole): branchd manages
// pods in its own namespace only.
func TestHelmDeployerRoleIsNamespaced(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kind: Role") {
		t.Error("chart did not render a namespaced Role")
	}
	if strings.Contains(out, "kind: ClusterRole") {
		t.Error("chart rendered a ClusterRole; deployer RBAC must stay namespaced")
	}
}
