package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestUtilityImageMatchesDockerfiles pins UtilityImage to the alpine base the
// Dockerfiles' runtime stage uses, so a base-image bump (Dependabot touches
// only the Dockerfiles) cannot leave the runtime helpers on an older image.
func TestUtilityImageMatchesDockerfiles(t *testing.T) {
	if !strings.Contains(UtilityImage, "@sha256:") {
		t.Fatalf("UtilityImage %q is not pinned by digest", UtilityImage)
	}
	from := regexp.MustCompile(`(?m)^FROM\s+(alpine:\S+)\s*$`)
	for _, df := range []string{"Dockerfile", "Dockerfile.ghook"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", df))
		if err != nil {
			t.Fatal(err)
		}
		m := from.FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s has no `FROM alpine:<tag>@sha256:<digest>` runtime stage", df)
		}
		if got := string(m[1]); got != UtilityImage {
			t.Errorf("%s runs on %s but runtime.UtilityImage is %s; bump them together", df, got, UtilityImage)
		}
	}
}
