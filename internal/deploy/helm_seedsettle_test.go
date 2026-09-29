package deploy

import (
	"strings"
	"testing"
)

// seedSettle maps to branchd --seed-settle, freeze by default, and the chart
// refuses a value branchd would refuse at startup.
func TestHelmSeedSettleArg(t *testing.T) {
	out, err := helmTemplate(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if !strings.Contains(out, "- --seed-settle=freeze") {
		t.Error("default render lacks --seed-settle=freeze")
	}
	for _, mode := range []string{"recover", "off"} {
		out, err := helmTemplate(t, "seedSettle="+mode)
		if err != nil {
			t.Fatalf("helm template seedSettle=%s: %v\n%s", mode, err, out)
		}
		if !strings.Contains(out, "- --seed-settle="+mode) {
			t.Errorf("seedSettle=%s did not render the branchd arg", mode)
		}
	}
	out, err = helmTemplate(t, "seedSettle=sometimes")
	if err == nil || !strings.Contains(out, "seedSettle must be freeze, recover or off") {
		t.Fatalf("seedSettle=sometimes rendered (err %v):\n%s", err, out)
	}
}
