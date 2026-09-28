package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

// exitStatus runs the CLI like cmd/pgb does and returns the exit status.
func exitStatus(t *testing.T, args ...string) int {
	t.Helper()
	root := NewRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	return ExitCode(cmd, err)
}

// planServer answers GET /v1/reconcile/plan with status and body.
func planServer(t *testing.T, status int, body any) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// CLI-03: drift and "could not compute the plan" used to share exit 1, so a
// CI gate could not tell real drift from an unreachable branchd or a bad
// token.
func TestDoctorExitCodes(t *testing.T) {
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	drift := engine.ReconcilePlan{Actions: []engine.Action{{Kind: engine.ActionFailStuck, Target: "b", Reason: "stuck"}}}
	cases := map[string]struct {
		args []string
		want int
	}{
		"clean":          {[]string{"doctor", "--server", planServer(t, http.StatusOK, engine.ReconcilePlan{})}, 0},
		"drift":          {[]string{"doctor", "--server", planServer(t, http.StatusOK, drift)}, ExitDrift},
		"unauthorized":   {[]string{"doctor", "--server", planServer(t, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})}, ExitDoctorFailed},
		"server error":   {[]string{"doctor", "--server", planServer(t, http.StatusInternalServerError, map[string]string{"error": "internal error"})}, ExitDoctorFailed},
		"bad server URL": {[]string{"doctor", "--server", "localhost:7070"}, ExitDoctorFailed},
		"bad flag":       {[]string{"doctor", "--stuck-timeout", "soon", "--server", "http://127.0.0.1:1"}, ExitDoctorFailed},
		"extra argument": {[]string{"doctor", "extra", "--server", "http://127.0.0.1:1"}, ExitDoctorFailed},
		// other commands keep exit 1 for every failure
		"gc failure":      {[]string{"gc", "--server", planServer(t, http.StatusInternalServerError, map[string]string{"error": "x"})}, ExitError},
		"unknown command": {[]string{"doktor"}, ExitError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := exitStatus(t, tc.args...); got != tc.want {
				t.Fatalf("pgb %v exited %d, want %d", tc.args, got, tc.want)
			}
		})
	}
	if ExitDrift == ExitDoctorFailed {
		t.Fatal("drift and failure must have distinct exit codes")
	}
}
