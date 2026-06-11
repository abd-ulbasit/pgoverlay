package engine

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// Unit tests for per-branch credential rotation (WithCredentialRotation):
// after masking and before the branch is marked ready, the engine ALTERs the
// role's password inside the branch via the in-container psql path and
// persists the new password on the branch row. Inherit mode (default) does
// neither. Reset re-rotates; freeze restarts of the parent never re-rotate.

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// alterRoleExecs returns the recorded psql execs that ran ALTER ROLE.
func alterRoleExecs(d *fakeDriver) [][]string {
	var out [][]string
	for _, c := range d.psqlExecs() {
		if strings.HasPrefix(c[len(c)-1], "ALTER ROLE") {
			out = append(out, c)
		}
	}
	return out
}

func TestRotateCredentialsOnCreate(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithCredentialRotation())
	src := readySource(t, r)
	if err := r.SetMaskScripts(src.ID, []registry.MaskScript{{Name: "m.sql", SQL: "UPDATE t SET x=1"}}); err != nil {
		t.Fatal(err)
	}

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != registry.BranchReady {
		t.Fatalf("state=%q", b.State)
	}
	if !hex32.MatchString(b.Password) {
		t.Fatalf("Password=%q want 32 hex chars", b.Password)
	}
	alters := alterRoleExecs(d)
	if len(alters) != 1 {
		t.Fatalf("ALTER ROLE execs=%d want 1: %v", len(alters), alters)
	}
	sql := alters[0][len(alters[0])-1]
	if want := `ALTER ROLE "postgres" WITH PASSWORD '` + b.Password + `'`; sql != want {
		t.Fatalf("ALTER ROLE sql=%q want %q", sql, want)
	}
	// rotation runs AFTER masking: the mask exec precedes the ALTER ROLE
	maskIdx, alterIdx := -1, -1
	for i, c := range d.execs {
		if c[0] != "psql" {
			continue
		}
		switch sql := c[len(c)-1]; {
		case strings.HasPrefix(sql, "UPDATE t"):
			maskIdx = i
		case strings.HasPrefix(sql, "ALTER ROLE"):
			alterIdx = i
		}
	}
	if maskIdx == -1 || alterIdx == -1 || alterIdx < maskIdx {
		t.Fatalf("rotation must run after masking: mask=%d alter=%d execs=%v", maskIdx, alterIdx, d.execs)
	}
}

func TestRotateCredentialsUsesSourceUser(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithCredentialRotation())
	s := &registry.Source{Name: "main", PGVersion: "17", Volume: "v", ConnUser: "app", ConnDB: "appdb"}
	if err := r.CreateSource(s); err != nil {
		t.Fatal(err)
	}
	if err := r.SetSourceState(s.ID, registry.SourceReady, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBranch(context.Background(), "pr-1", "main", 0); err != nil {
		t.Fatal(err)
	}
	alters := alterRoleExecs(d)
	if len(alters) != 1 {
		t.Fatalf("ALTER ROLE execs=%v", alters)
	}
	if sql := alters[0][len(alters[0])-1]; !strings.HasPrefix(sql, `ALTER ROLE "app" WITH PASSWORD`) {
		t.Fatalf("ALTER ROLE sql=%q want role \"app\"", sql)
	}
}

func TestInheritModeStoresNoPasswordAndDoesNotExec(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d) // rotation OFF (default)
	readySource(t, r)

	b, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Password != "" {
		t.Fatalf("inherit mode stored a password: %q", b.Password)
	}
	if alters := alterRoleExecs(d); len(alters) != 0 {
		t.Fatalf("inherit mode ran ALTER ROLE: %v", alters)
	}
}

func TestResetReRotates(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithCredentialRotation())
	readySource(t, r)

	b1, err := e.CreateBranch(context.Background(), "pr-1", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := e.ResetBranch(context.Background(), "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if !hex32.MatchString(b2.Password) {
		t.Fatalf("reset Password=%q want 32 hex chars", b2.Password)
	}
	if b2.Password == b1.Password {
		t.Fatal("reset must rotate a NEW password")
	}
	if n := len(alterRoleExecs(d)); n != 2 {
		t.Fatalf("ALTER ROLE execs=%d want 2 (create + reset)", n)
	}
}

// A freeze (overlay branch-from-branch) restarts the parent on a fresh rw
// volume; that restart must NOT re-rotate the parent — only the child gets a
// fresh password.
func TestFreezeRotatesChildKeepsParentPassword(t *testing.T) {
	d := newFake()
	e, r := testEngine(t, d, WithCredentialRotation())
	readySource(t, r)

	p, err := e.CreateBranch(context.Background(), "p", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.CreateBranchFrom(context.Background(), "c", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hex32.MatchString(c.Password) {
		t.Fatalf("child Password=%q want 32 hex chars", c.Password)
	}
	if c.Password == p.Password {
		t.Fatal("child must get its own password, not the parent's")
	}
	pAfter, err := r.GetBranchByName("p")
	if err != nil {
		t.Fatal(err)
	}
	if pAfter.Password != p.Password {
		t.Fatalf("parent password changed across the freeze: %q -> %q", p.Password, pAfter.Password)
	}
	if n := len(alterRoleExecs(d)); n != 2 {
		t.Fatalf("ALTER ROLE execs=%d want 2 (parent create + child create; the freeze restart must not rotate)", n)
	}
}

// CSI branch-from-branch quiesces and restarts the parent around the PVC
// clone; same contract: child rotated, parent password untouched.
func TestCSICloneRotatesChildKeepsParentPassword(t *testing.T) {
	d := newFake()
	e, r := csiEngine(t, d, WithCredentialRotation())
	readySource(t, r)

	p, err := e.CreateBranch(context.Background(), "p", "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := e.CreateBranchFrom(context.Background(), "c", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hex32.MatchString(c.Password) || c.Password == p.Password {
		t.Fatalf("child Password=%q parent=%q", c.Password, p.Password)
	}
	pAfter, _ := r.GetBranchByName("p")
	if pAfter.Password != p.Password {
		t.Fatalf("parent password changed across the clone: %q -> %q", p.Password, pAfter.Password)
	}
	if n := len(alterRoleExecs(d)); n != 2 {
		t.Fatalf("ALTER ROLE execs=%d want 2", n)
	}
}
