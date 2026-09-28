// Offline check that deploy/preview-deployer-rbac.yaml can actually install
// the chart (no cluster; skipped without the helm binary). Two things used to
// stop it at the first API call:
//
//   - RBAC escalation prevention: a subject may only create a Role whose rules
//     it holds itself, and the chart creates branchd's Role (pods/exec,
//     pods/log, secrets, and per mode PVCs, VolumeSnapshots, Leases).
//   - Helm's release storage LISTs Secrets by label on every install/upgrade.
package deploy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// decodeManifests splits a multi-document YAML stream into objects.
func decodeManifests(t *testing.T, raw []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("decode manifests: %v", err)
		}
		if len(m) == 0 {
			continue // empty document (comments only)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
}

// roleRules returns the rules of every Role among objs.
func roleRules(t *testing.T, objs []*unstructured.Unstructured) []rbacv1.PolicyRule {
	t.Helper()
	var rules []rbacv1.PolicyRule
	for _, o := range objs {
		if o.GetKind() != "Role" {
			continue
		}
		var r rbacv1.Role
		if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &r); err != nil {
			t.Fatalf("convert Role %s: %v", o.GetName(), err)
		}
		rules = append(rules, r.Rules...)
	}
	return rules
}

// allows reports whether rules grant verb on group/resource for any object
// name (a resourceNames-restricted rule does not count: it cannot authorize
// creates, lists, or a Role rule that is not itself name-restricted).
func allows(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	match := func(list []string, v string) bool { return slices.Contains(list, v) || slices.Contains(list, "*") }
	for _, r := range rules {
		if len(r.ResourceNames) == 0 && match(r.APIGroups, group) && match(r.Resources, resource) && match(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// kindResource maps the kinds the chart renders to their API group/resource.
var kindResource = map[string][2]string{
	"Deployment":            {"apps", "deployments"},
	"Service":               {"", "services"},
	"Secret":                {"", "secrets"},
	"ServiceAccount":        {"", "serviceaccounts"},
	"PersistentVolumeClaim": {"", "persistentvolumeclaims"},
	"Role":                  {"rbac.authorization.k8s.io", "roles"},
	"RoleBinding":           {"rbac.authorization.k8s.io", "rolebindings"},
	"NetworkPolicy":         {"networking.k8s.io", "networkpolicies"},
}

func TestPreviewDeployerCanInstallChart(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "preview-deployer-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deployer := roleRules(t, decodeManifests(t, raw))
	if len(deployer) == 0 {
		t.Fatal("no Role in deploy/preview-deployer-rbac.yaml")
	}

	// Helm's default (secret) release storage: history is a label-selected
	// list, then get/create/update of the revision Secrets, delete to prune.
	for _, verb := range []string{"list", "get", "create", "update", "delete"} {
		if !allows(deployer, "", "secrets", verb) {
			t.Errorf("deployer cannot %s secrets, which Helm's release storage needs", verb)
		}
	}

	modes := map[string][]string{
		"hostpath": nil,
		"csi+snapshots+ha+netpol+ghook": {
			"storage.mode=csi", "storage.storageClass=fast-clone", "storage.snapshotClass=snap",
			"replicaCount=2", "networkPolicy.enabled=true",
			"ghook.enabled=true", "ghook.webhookSecret=w", "ghook.source=main",
		},
	}
	for mode, sets := range modes {
		out, err := helmTemplate(t, sets...)
		if err != nil {
			t.Fatalf("%s: helm template: %v\n%s", mode, err, out)
		}
		objs := decodeManifests(t, []byte(out))

		// Escalation prevention: every rule of the chart's Role must be held.
		for _, rule := range roleRules(t, objs) {
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					for _, verb := range rule.Verbs {
						if !allows(deployer, g, res, verb) {
							t.Errorf("%s: chart Role grants %s on %q/%s, which the deployer does not hold; "+
								"Kubernetes will refuse to let it create that Role", mode, verb, g, res)
						}
					}
				}
			}
		}
		// And it must be able to manage every object the chart renders
		// (install creates, upgrade gets and patches, uninstall deletes, and
		// `helm --wait` lists and watches).
		for _, o := range objs {
			gr, ok := kindResource[o.GetKind()]
			if !ok {
				t.Errorf("%s: chart renders a %s; add it to kindResource and to the deployer Role", mode, o.GetKind())
				continue
			}
			for _, verb := range []string{"create", "get", "patch", "delete", "list", "watch"} {
				if !allows(deployer, gr[0], gr[1], verb) {
					t.Errorf("%s: deployer cannot %s %s (%q/%s) that the chart renders", mode, verb, o.GetKind(), gr[0], gr[1])
				}
			}
		}
	}
}
