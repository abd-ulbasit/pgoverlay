package deploy

import (
	"strings"
	"testing"
)

// cow.lazyrw maps to branchd --lazyrw, on by default (also when a values file
// leaves the cow block out), and the chart refuses a value that is not a
// boolean instead of guessing.
func TestHelmLazyRWArg(t *testing.T) {
	for _, c := range []struct {
		sets []string
		want string
	}{
		{nil, "- --lazyrw=on"},
		{[]string{"cow.lazyrw=true"}, "- --lazyrw=on"},
		{[]string{"cow.lazyrw=false"}, "- --lazyrw=off"},
		{[]string{"cow=null"}, "- --lazyrw=on"},
		{csiSets(), "- --lazyrw=on"},
	} {
		out, err := helmTemplate(t, c.sets...)
		if err != nil {
			t.Fatalf("helm template %v: %v\n%s", c.sets, err, out)
		}
		if strings.Count(out, "- --lazyrw=") != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%v: want exactly one %q in the branchd args", c.sets, c.want)
		}
	}
	out, err := helmTemplate(t, "cow.lazyrw=sometimes")
	if err == nil || !strings.Contains(out, "cow.lazyrw must be true or false") {
		t.Fatalf("cow.lazyrw=sometimes rendered (err %v):\n%s", err, out)
	}
}
