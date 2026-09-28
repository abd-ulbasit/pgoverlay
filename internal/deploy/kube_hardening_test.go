// Offline helm-template tests for the chart's kube hardening: where branchd is
// pinned, what the internet-facing ghook pod gets, and which pods the
// NetworkPolicies let reach the Postgres source. Skipped without helm.
package deploy

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// renderObjects renders the chart and decodes it.
func renderObjects(t *testing.T, sets ...string) []*unstructured.Unstructured {
	t.Helper()
	out, err := helmTemplate(t, sets...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", sets, err, out)
	}
	return decodeManifests(t, []byte(out))
}

// find returns the rendered object of kind whose name ends in suffix.
func find(t *testing.T, objs []*unstructured.Unstructured, kind, suffix string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && strings.HasSuffix(o.GetName(), suffix) {
			return o
		}
	}
	t.Fatalf("no %s *%s rendered", kind, suffix)
	return nil
}

// KUBE-11: in csi mode with the registry on a PVC nothing ties branchd to a
// node, so it must not be pinned with nodeName (a replaced node would strand
// it Pending forever). A hostPath state dir (hostpath mode, or csi with
// persistence off) keeps the pin.
func TestHelmBranchdNodePin(t *testing.T) {
	cases := []struct {
		name string
		sets []string
		want string // spec.nodeName, "" = unpinned
	}{
		{"hostpath", nil, "storage-1"},
		{"csi", csiSets(), ""},
		{"csi-no-persistence", csiSets("persistence.enabled=false"), "storage-1"},
		{"hostpath-with-persistence", []string{"persistence.enabled=true"}, "storage-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := find(t, renderObjects(t, c.sets...), "Deployment", "pgoverlay")
			got, _, _ := unstructured.NestedString(d.Object, "spec", "template", "spec", "nodeName")
			if got != c.want {
				t.Errorf("nodeName = %q, want %q", got, c.want)
			}
		})
	}
}

// KUBE-07: the webhook receiver gets no ServiceAccount token, and uses the
// operator token it is given instead of branchd's admin token.
func TestHelmGhookLeastPrivilege(t *testing.T) {
	ghook := []string{"ghook.enabled=true", "ghook.webhookSecret=w", "ghook.source=main"}
	tokenRef := func(objs []*unstructured.Unstructured) (name, key string) {
		d := find(t, objs, "Deployment", "-ghook")
		cs, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
		env, _, _ := unstructured.NestedSlice(cs[0].(map[string]any), "env")
		for _, e := range env {
			e := e.(map[string]any)
			if e["name"] == "GHOOK_PGOVERLAY_TOKEN" {
				name, _, _ = unstructured.NestedString(e, "valueFrom", "secretKeyRef", "name")
				key, _, _ = unstructured.NestedString(e, "valueFrom", "secretKeyRef", "key")
			}
		}
		return name, key
	}

	objs := renderObjects(t, ghook...)
	d := find(t, objs, "Deployment", "-ghook")
	if v, found, _ := unstructured.NestedBool(d.Object, "spec", "template", "spec", "automountServiceAccountToken"); !found || v {
		t.Errorf("ghook automountServiceAccountToken = %v (set %v), want false", v, found)
	}
	if name, key := tokenRef(objs); name != "test-release-pgoverlay-token" || key != "token" {
		t.Errorf("fallback token ref = %s/%s, want the admin token secret", name, key)
	}

	objs = renderObjects(t, append(ghook, "ghook.apiTokenSecret=ghook-api", "ghook.apiTokenKey=operator")...)
	if name, key := tokenRef(objs); name != "ghook-api" || key != "operator" {
		t.Errorf("token ref = %s/%s, want ghook-api/operator", name, key)
	}
}

// KUBE-09/15: branch pods never talk to the Postgres source, so their policy
// opens DNS only; sourceEgress is applied to the seed helper pods instead.
func TestHelmNetworkPolicySourceEgressOnHelpersOnly(t *testing.T) {
	objs := renderObjects(t, "networkPolicy.enabled=true", "networkPolicy.sourceEgress[0].ipBlock.cidr=10.0.5.10/32")
	peers := func(np *unstructured.Unstructured) string {
		egress, _, _ := unstructured.NestedSlice(np.Object, "spec", "egress")
		var b strings.Builder
		for _, e := range egress {
			to, _, _ := unstructured.NestedSlice(e.(map[string]any), "to")
			for _, p := range to {
				cidr, _, _ := unstructured.NestedString(p.(map[string]any), "ipBlock", "cidr")
				b.WriteString(cidr + " ")
			}
		}
		return b.String()
	}
	branch := find(t, objs, "NetworkPolicy", "-branch-pods")
	if got := peers(branch); strings.Contains(got, "10.0.5.10") {
		t.Errorf("branch pods may reach the source: egress peers %q", got)
	}
	helper := find(t, objs, "NetworkPolicy", "-helper-pods")
	if role, _, _ := unstructured.NestedString(helper.Object, "spec", "podSelector", "matchLabels", "pgoverlay.role"); role != "helper" {
		t.Errorf("helper policy selects role %q", role)
	}
	if got := peers(helper); !strings.Contains(got, "10.0.5.10/32") {
		t.Errorf("helper pods cannot reach the source: egress peers %q", got)
	}
}
