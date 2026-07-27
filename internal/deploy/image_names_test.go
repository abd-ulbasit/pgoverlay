// Guards the one thing the pgbranch -> pgoverlay rename could have broken
// silently: the container image name is written down in two places that must
// agree, and nothing used to check that they did.
//
// The Helm chart's values.yaml is the source of truth — it is what a real
// deployment pulls. The Makefile's `docker build -t ...` targets are what
// produce that image locally, and the helm ITs install with
// image.pullPolicy=Never, so a pod can only start from an image already
// side-loaded into the kind node. If the Makefile and the chart ever name
// different images, `make docker-build && helm install` leaves every pod in
// ErrImageNeverPull and the only visible symptom is Helm reporting
// "Available: 0/1" three minutes later, with no mention of an image anywhere.
//
// The ITs themselves no longer hardcode the name at all (helm_it_test.go's
// chartImage renders the chart to learn it), so this test covers the
// remaining pair. It needs no cluster, no helm binary and no network, so it
// runs in the plain `unit` job on every push rather than only in `kube`.
package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// makefileImages returns every image reference passed to `docker build -t` in
// the Makefile, keyed by the target that builds it.
func makefileImages(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	// e.g. "docker-build:\n\tdocker build -t ghcr.io/... ." — capture the
	// target name and the -t argument of the recipe line beneath it.
	re := regexp.MustCompile(`(?m)^(docker-build[\w-]*):\n\tdocker build .*?-t (\S+)`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("no `docker build -t` targets found in the Makefile; this guard has gone blind")
	}
	return out
}

// valuesImage extracts "<repository>:<tag>" from a values.yaml image block at
// the given indentation, e.g. indent 2 for the top-level `image:` block and
// indent 4 for `ghook.image`.
func valuesImage(t *testing.T, values, block string, indent int) string {
	t.Helper()
	pad := fmt.Sprintf(`[ ]{%d}`, indent)
	// The block header sits one indent level shallower than its keys.
	re := regexp.MustCompile(
		`(?m)^[ ]{` + fmt.Sprint(indent-2) + `}` + regexp.QuoteMeta(block) + `:\n` +
			`(?:` + pad + `#.*\n)*` +
			pad + `repository:[ ]+(\S+)\n` +
			`(?:` + pad + `#.*\n)*` +
			pad + `tag:[ ]+(\S+)\n`)
	m := re.FindStringSubmatch(values)
	if m == nil {
		t.Fatalf("could not find a %q image block (repository+tag) at indent %d in values.yaml; "+
			"the file's shape changed and this guard can no longer read it", block, indent)
	}
	return m[1] + ":" + m[2]
}

// TestMakefileImagesMatchChartDefaults fails when `make docker-build` would
// produce an image the chart does not deploy (or vice versa).
func TestMakefileImagesMatchChartDefaults(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "helm", "pgoverlay", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values := string(b)
	mk := makefileImages(t)

	for _, tc := range []struct {
		target string
		block  string
		indent int
	}{
		{"docker-build", "image", 2},
		{"docker-build-ghook", "image", 4},
	} {
		t.Run(tc.target, func(t *testing.T) {
			got, ok := mk[tc.target]
			if !ok {
				t.Fatalf("Makefile has no %s target; targets found: %v", tc.target, mk)
			}
			if want := valuesImage(t, values, tc.block, tc.indent); got != want {
				t.Errorf("image drift: Makefile %s builds %q but the chart deploys %q.\n"+
					"With image.pullPolicy=Never (the helm ITs) a pod can only start from a "+
					"side-loaded image, so this drift strands every pod in ErrImageNeverPull.",
					tc.target, got, want)
			}
		})
	}
}

// TestChartImageIsTheBranchdImage covers the derivation the helm ITs rely on:
// chartImage renders the chart to decide what to `docker build` and `kind
// load`. If it ever returned something other than the branchd image the chart
// deploys (a stray `image:` line rendering first, say), the ITs would
// side-load the wrong thing and every pod would stall in ErrImageNeverPull.
// Asserting it here means that shows up in a one-second unit test instead of a
// three-minute Helm timeout in the kube job.
//
// Needs the helm binary but no cluster; the kube job always has it, and
// helm_template_test.go already uses this same skip for the offline renders.
func TestChartImageIsTheBranchdImage(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "helm", "pgoverlay", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := chartImage(t), valuesImage(t, string(b), "image", 2); got != want {
		t.Errorf("chartImage() = %q, want the chart's branchd image %q", got, want)
	}
}
