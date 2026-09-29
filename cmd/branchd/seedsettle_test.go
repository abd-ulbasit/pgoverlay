package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestSeedSettleFlagValidation re-runs the test binary as branchd with an
// unknown --seed-settle (or $PGOVERLAY_SEED_SETTLE): it must refuse to start,
// naming the valid modes, before it looks at PGOVERLAY_TOKEN, the registry or
// a container runtime.
func TestSeedSettleFlagValidation(t *testing.T) {
	if os.Getenv("BRANCHD_SEED_SETTLE_CHILD") == "1" {
		os.Args = append([]string{"branchd"}, strings.Fields(os.Getenv("BRANCHD_ARGS"))...)
		if err := run(); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(3)
		}
		os.Exit(0)
	}
	for _, tc := range []struct{ args, env string }{
		{args: "--seed-settle=sometimes"},
		{env: "sometimes"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSeedSettleFlagValidation$")
		cmd.Env = append(os.Environ(), "BRANCHD_SEED_SETTLE_CHILD=1", "BRANCHD_ARGS="+tc.args,
			"PGOVERLAY_SEED_SETTLE="+tc.env, "PGOVERLAY_TOKEN=", "PGOVERLAY_HOME="+t.TempDir())
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if err == nil || !errors.As(err, &ee) || ee.ExitCode() != 3 {
			t.Fatalf("%+v: branchd exited %v, want a startup error (output %q)", tc, err, out)
		}
		for _, want := range []string{"--seed-settle", `"sometimes"`, "want freeze, recover or off"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("%+v: output %q lacks %q", tc, out, want)
			}
		}
		// refused before the token check, so the error is about the flag
		if strings.Contains(string(out), "PGOVERLAY_TOKEN") {
			t.Errorf("%+v: validated after the token: %q", tc, out)
		}
	}
}

func TestEnvString(t *testing.T) {
	t.Setenv("PGOVERLAY_TEST_ENVSTRING", "")
	if got := envString("PGOVERLAY_TEST_ENVSTRING", "freeze"); got != "freeze" {
		t.Fatalf("unset: %q", got)
	}
	t.Setenv("PGOVERLAY_TEST_ENVSTRING", "off")
	if got := envString("PGOVERLAY_TEST_ENVSTRING", "freeze"); got != "off" {
		t.Fatalf("set: %q", got)
	}
}
