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
