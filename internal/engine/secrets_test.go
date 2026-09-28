package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// argvErrDriver fails the ALTER ROLE exec the way both real drivers do: with
// the full argv (and so the new password) formatted into the error.
type argvErrDriver struct {
	*fakeDriver
	password string // the password the failed ALTER ROLE carried
}

func (d *argvErrDriver) Exec(ctx context.Context, id string, cmd []string) error {
	if len(cmd) > 0 && strings.HasPrefix(cmd[len(cmd)-1], "ALTER ROLE") {
		stmt := cmd[len(cmd)-1]
		d.password = stmt[strings.LastIndex(stmt[:len(stmt)-1], "'")+1 : len(stmt)-1]
		return fmt.Errorf("exec %v exited 2: psql: error: connection failed", cmd)
	}
	return d.fakeDriver.Exec(ctx, id, cmd)
}

var _ runtime.Driver = (*argvErrDriver)(nil)

// SECRETS-11: a failed credential rotation must not leak the new password
// into the returned error or the failed transition's reason.
func TestRotateFailureRedactsPassword(t *testing.T) {
	d := &argvErrDriver{fakeDriver: newFake()}
	e, r := testEngine(t, d, WithCredentialRotation())
	readySource(t, r)

	_, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err == nil {
		t.Fatal("create succeeded although ALTER ROLE failed")
	}
	if d.password == "" || len(d.password) != 32 {
		t.Fatalf("test driver did not capture the password (%q)", d.password)
	}
	if strings.Contains(err.Error(), d.password) {
		t.Fatalf("error leaks the rotated password: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "exited 2") {
		t.Fatalf("error lost its diagnostic text: %v", err)
	}
	hist, herr := r.BranchHistory("pr-1")
	if herr != nil {
		t.Fatal(herr)
	}
	for _, tr := range hist {
		if strings.Contains(tr.Reason, d.password) {
			t.Fatalf("transition reason leaks the rotated password: %q", tr.Reason)
		}
	}
}

func TestRedactSecretKeepsErrorsIs(t *testing.T) {
	cause := fmt.Errorf("exec [psql -c ALTER ROLE x PASSWORD 'hunter2hunter2']: %w", context.Canceled)
	err := redactSecret(cause, "hunter2hunter2")
	if strings.Contains(err.Error(), "hunter2hunter2") {
		t.Fatalf("not redacted: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("redaction broke errors.Is")
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("redacted error unwraps to the unredacted one")
	}
	if plain := errors.New("no secret here"); redactSecret(plain, "hunter2hunter2") != plain {
		t.Fatal("error without the secret should pass through unchanged")
	}
}

// With rotation off, a reset clone carries the source's credentials, so a
// password stored by an earlier rotating run (readable or not) is cleared
// instead of being handed out.
func TestInheritModeResetClearsStalePassword(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d) // rotation off
	readySource(t, r)
	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetBranchPassword(b.ID, "stale-rotated-password"); err != nil {
		t.Fatal(err)
	}
	got, err := e.ResetBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "" || got.PasswordUnavailable {
		t.Fatalf("after inherit-mode reset: password=%q unavailable=%v, want cleared", got.Password, got.PasswordUnavailable)
	}
	if n := len(alterRoleExecs(d)); n != 0 {
		t.Fatalf("inherit mode ran ALTER ROLE %d times", n)
	}
}
