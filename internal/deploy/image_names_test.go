// Guards the container image name, which is written down in several places
// that have to agree, and nothing used to check that they did.
//
// The Helm chart is the source of truth — it is what a real deployment pulls.
// It names the image in two halves that are deliberately NOT symmetric:
//
//   - the repository (values.yaml `image.repository`) must match what the
//     Makefile's `docker build -t` targets produce. This is the half the
//     pgbranch -> pgoverlay rename could have broken silently, and it is the
//     half that strands pods in ErrImageNeverPull: the helm ITs install with
//     image.pullPolicy=Never, so a pod can only start from an image already
//     side-loaded into the kind node, and the only visible symptom of drift is
//     Helm reporting "Available: 0/1" three minutes later with no mention of
//     an image anywhere.
//
//   - the tag diverges on purpose. The Makefile builds `:dev` for the local
//     side-load path; the chart defaults to `""`, the sentinel meaning "use
//     Chart.yaml's appVersion", which is a tag actually pushed to GHCR. That
//     split is the fix for the launch-day bug where the chart defaulted to
//     `:dev` and a plain `helm install` from the README pulled a tag that had
//     never been pushed anywhere, landing in ImagePullBackOff. So instead of
//     asserting the two tags are equal (they no longer are), each side is
//     pinned to its own documented constant, which is strictly more specific.
//
// The ITs themselves no longer hardcode the name at all (helm_it_test.go's
// chartImage renders the chart to learn it), so this file covers the rest.
// Everything but TestChartImageIsTheBranchdImage needs no cluster, no helm
// binary and no network, so it runs in the plain `unit` job on every push
// rather than only in `kube`.
package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// localTag is the tag `make docker-build` produces for the side-load path. It
// is spelled in the Makefile, in README.md, in docs/kubernetes.md and in
// hack/helm-test.sh, so it is a constant of the repo rather than an
// implementation detail of either side.
const localTag = "dev"

// followChartTag is the values.yaml sentinel meaning "use Chart.yaml's
// appVersion". Rendered by `{{ .Values.image.tag | default .Chart.AppVersion }}`.
const followChartTag = ""

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

// splitImage splits "<repository>:<tag>" on the LAST colon, so a registry host
// carrying a port (localhost:5000/foo:dev) does not split in the wrong place.
func splitImage(t *testing.T, ref string) (repo, tag string) {
	t.Helper()
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		t.Fatalf("image reference %q has no tag; this guard compares tags and cannot read it", ref)
	}
	return ref[:i], ref[i+1:]
}

// valuesImageParts extracts the repository and tag from a values.yaml image
// block at the given indentation, e.g. indent 2 for the top-level `image:`
// block and indent 4 for `ghook.image`. The tag is returned with any
// surrounding quotes stripped, so the `tag: ""` sentinel comes back as the
// empty string rather than as two literal quote characters.
func valuesImageParts(t *testing.T, values, block string, indent int) (repo, tag string) {
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
	return m[1], strings.Trim(m[2], `"'`)
}

func valuesImageRepo(t *testing.T, values, block string, indent int) string {
	t.Helper()
	repo, _ := valuesImageParts(t, values, block, indent)
	return repo
}

func valuesImageTag(t *testing.T, values, block string, indent int) string {
	t.Helper()
	_, tag := valuesImageParts(t, values, block, indent)
	return tag
}

func chartValues(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "helm", "pgoverlay", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// chartAppVersion parses Chart.yaml textually. Deliberately does NOT shell out
// to helm: it is the independent second opinion that
// TestChartImageIsTheBranchdImage compares a real render against, so it must
// not share a code path with the render.
func chartAppVersion(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "helm", "pgoverlay", "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^appVersion:[ ]+(\S+)[ ]*$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no appVersion in Chart.yaml; the chart's default image tag comes from it, " +
			"so this guard can no longer read what the chart deploys")
	}
	return strings.Trim(m[1], `"'`)
}

// TestMakefileAndChartAgreeOnImages fails when `make docker-build` and the
// chart disagree about the image REPOSITORY, and when either side's tag drifts
// off the constant it is supposed to hold. See the file comment for why the
// tags are asserted separately rather than against each other.
func TestMakefileAndChartAgreeOnImages(t *testing.T) {
	values := chartValues(t)
	mk := makefileImages(t)

	for _, tc := range []struct {
		target string
		block  string // the YAML key, which is "image" at both nesting levels
		path   string // ...so spell the dotted path too, for readable failures
		indent int
	}{
		{"docker-build", "image", "image", 2},
		{"docker-build-ghook", "image", "ghook.image", 4},
	} {
		t.Run(tc.target, func(t *testing.T) {
			ref, ok := mk[tc.target]
			if !ok {
				t.Fatalf("Makefile has no %s target; targets found: %v", tc.target, mk)
			}
			mkRepo, mkTag := splitImage(t, ref)
			chartRepo, chartTag := valuesImageParts(t, values, tc.block, tc.indent)

			if mkRepo != chartRepo {
				t.Errorf("image repository drift: Makefile %s builds %q but the chart deploys %q.\n"+
					"With image.pullPolicy=Never (the helm ITs) a pod can only start from a "+
					"side-loaded image, so this drift strands every pod in ErrImageNeverPull.",
					tc.target, mkRepo, chartRepo)
			}
			if mkTag != localTag {
				t.Errorf("Makefile %s builds tag %q, want %q. The local side-load path is "+
					"named as %q by README.md, docs/kubernetes.md and hack/helm-test.sh; "+
					"change all of them together or none.", tc.target, mkTag, localTag, localTag)
			}
			if chartTag != followChartTag {
				t.Errorf("values.yaml %s.tag is %q, want the empty sentinel meaning "+
					"\"use Chart.yaml's appVersion\".\nPinning a tag here is the launch-day "+
					"bug regressing: %q is only ever built locally by `make %s` and "+
					"side-loaded, never pushed, so a plain `helm install` from the README "+
					"lands in ImagePullBackOff.", tc.path, chartTag, chartTag, tc.target)
			}
		})
	}
}

// TestChartImageIsTheBranchdImage covers the derivation the helm ITs rely on:
// chartImage renders the chart to decide what to `docker build` and `kind
// load`. If it ever returned something other than the branchd image the chart
// deploys (a stray `image:` line rendering first, say — a sidecar, an
// initContainer, or ghook flipped on by default), the ITs would side-load the
// wrong thing and every pod would stall in ErrImageNeverPull. Asserting it
// here means that shows up in a one-second unit test instead of a three-minute
// Helm timeout in the kube job.
//
// The two sides stay on independent paths so this cannot go tautological: the
// left is a real `helm template` render, the right is values.yaml and
// Chart.yaml read as text, touching neither helm nor any template.
//
// Needs the helm binary but no cluster; the kube job always has it, and
// helm_template_test.go already uses this same skip for the offline renders.
func TestChartImageIsTheBranchdImage(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	want := valuesImageRepo(t, chartValues(t), "image", 2) + ":" + chartAppVersion(t)
	if got := chartImage(t); got != want {
		t.Errorf("chartImage() = %q, want the chart's branchd image %q "+
			"(values.yaml image.repository + Chart.yaml appVersion)", got, want)
	}
}

// TestChartAppVersionIsAPublishedTagShape pins appVersion to the shape of a
// tag this project actually publishes.
//
// Because values.yaml's `tag: ""` sentinel makes appVersion the literal image
// tag kubelet pulls, an appVersion that is merely valid SemVer is not enough:
// every git tag, GitHub release and GHCR tag in this repo is v-prefixed
// (v1, v1.0.0-rc.1 .. rc.4). Dropping the `v` at release time renders
// ghcr.io/abd-ulbasit/pgoverlay-branchd:1.0.0-rc.4 — syntactically fine, and
// nonexistent. That exact mistake has already been made once; this catches the
// next one in a one-second unit test instead of in a user's ImagePullBackOff.
//
// It checks the SHAPE, not existence. Proving the tag was pushed needs a
// registry round-trip, which this offline package deliberately does not do.
func TestChartAppVersionIsAPublishedTagShape(t *testing.T) {
	const shape = `^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`
	if got := chartAppVersion(t); !regexp.MustCompile(shape).MatchString(got) {
		t.Errorf("Chart.yaml appVersion = %q, want a v-prefixed release tag matching %s.\n"+
			"appVersion IS the default image tag (values.yaml image.tag is the empty "+
			"sentinel), so it must name a tag pushed to GHCR; every tag this repo has "+
			"ever published is v-prefixed.", got, shape)
	}
}

// TestGhookImageTagFollowsTheChart is the ghook half of the appVersion
// derivation. hack/helm-test.sh renders ghook and asserts the same string in
// the `helm` job; this asserts it without helm, in the `unit` job.
func TestGhookImageTagFollowsTheChart(t *testing.T) {
	if got := valuesImageTag(t, chartValues(t), "image", 4); got != followChartTag {
		t.Errorf("values.yaml ghook.image.tag = %q, want the empty sentinel so ghook "+
			"tracks Chart.yaml appVersion like branchd does", got)
	}
}
