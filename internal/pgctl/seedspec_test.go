package pgctl

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func validSpec() SeedSpec {
	return SeedSpec{Image: "postgres:17", Volume: "v", Host: "db.internal", Port: 5432, User: "postgres", Password: "pw"}
}

func TestSeedSpecValidate(t *testing.T) {
	t.Setenv(SSLModeEnv, "")
	cases := []struct {
		name   string
		mutate func(*SeedSpec)
		env    string
		want   string // "" = valid
	}{
		{"valid", func(*SeedSpec) {}, "", ""},
		{"empty host", func(s *SeedSpec) { s.Host = "" }, "", "host is empty"},
		{"blank host", func(s *SeedSpec) { s.Host = "  " }, "", "host is empty"},
		{"socket dir host", func(s *SeedSpec) { s.Host = "/var/run/postgresql" }, "", "Unix socket"},
		{"port zero", func(s *SeedSpec) { s.Port = 0 }, "", "out of range"},
		{"port too high", func(s *SeedSpec) { s.Port = 70000 }, "", "out of range"},
		{"empty user", func(s *SeedSpec) { s.User = "" }, "", "user is empty"},
		{"explicit sslmode", func(s *SeedSpec) { s.SSLMode = "verify-full" }, "", ""},
		{"bad sslmode", func(s *SeedSpec) { s.SSLMode = "on" }, "", `sslmode "on"`},
		{"sslmode from env", func(*SeedSpec) {}, "require", ""},
		{"bad sslmode from env", func(*SeedSpec) {}, "yes", `sslmode "yes"`},
		{"spec sslmode wins over env", func(s *SeedSpec) { s.SSLMode = "disable" }, "yes", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(SSLModeEnv, tc.env)
			s := validSpec()
			tc.mutate(&s)
			err := s.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want ErrInvalidSpec mentioning %q", err, tc.want)
			}
		})
	}
}

func TestSeedDumpSpecValidateSchemas(t *testing.T) {
	t.Setenv(SSLModeEnv, "")
	for _, tc := range []struct {
		schemas []string
		want    string
	}{
		{[]string{"public", "audit", "app_*"}, ""},
		{[]string{"public", ""}, "empty dump schema"},
		{[]string{" "}, "empty dump schema"},
		// the registry stores the list comma-joined: a refresh would split it
		{[]string{`"a,b"`}, "comma"},
	} {
		err := SeedDumpSpec{SeedSpec: validSpec(), Schemas: tc.schemas}.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("schemas %q: Validate() = %v, want nil", tc.schemas, err)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidSpec) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("schemas %q: Validate() = %v, want ErrInvalidSpec mentioning %q", tc.schemas, err, tc.want)
		}
	}
}

// An invalid spec fails before any helper container runs, so no seed volume
// is touched and the error names the real problem.
func TestSeedRejectsInvalidSpecBeforeHelpers(t *testing.T) {
	for name, run := range map[string]func(d *recordingDriver, s SeedSpec) error{
		"basebackup": func(d *recordingDriver, s SeedSpec) error { return Seed(context.Background(), d, s) },
		"dump": func(d *recordingDriver, s SeedSpec) error {
			return SeedDump(context.Background(), d, SeedDumpSpec{SeedSpec: s})
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := &recordingDriver{}
			s := validSpec()
			s.Host = ""
			if err := run(d, s); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
			if len(d.helpers) != 0 {
				t.Fatalf("ran %d helpers for an invalid spec", len(d.helpers))
			}
		})
	}
}

func envOf(env []string, key string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// The basebackup helper bounds its connect time and carries the sslmode:
// without PGCONNECT_TIMEOUT an unreachable host hung the seed for minutes.
func TestSeedBasebackupConnectionEnv(t *testing.T) {
	for _, tc := range []struct {
		name, spec, env, want string
	}{
		{"libpq default", "", "", "prefer"},
		{"from env", "", "require", "require"},
		{"from spec", "verify-full", "require", "verify-full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(SSLModeEnv, tc.env)
			d := &recordingDriver{}
			s := validSpec()
			s.SSLMode = tc.spec
			if err := Seed(context.Background(), d, s); err != nil {
				t.Fatal(err)
			}
			bb := d.helpers[1]
			if bb.Cmd[0] != "pg_basebackup" {
				t.Fatalf("helper[1] = %v, want pg_basebackup", bb.Cmd)
			}
			if v, _ := envOf(bb.Env, "PGSSLMODE"); v != tc.want {
				t.Errorf("PGSSLMODE = %q, want %q (env %v)", v, tc.want, bb.Env)
			}
			if v, _ := envOf(bb.Env, "PGCONNECT_TIMEOUT"); v != "10" {
				t.Errorf("PGCONNECT_TIMEOUT = %q, want 10", v)
			}
			if v, _ := envOf(bb.Env, "PGPASSWORD"); v != "pw" {
				t.Errorf("PGPASSWORD = %q, want the source password", v)
			}
			if strings.Contains(strings.Join(bb.Cmd, " "), "pw") {
				t.Errorf("password leaked into argv: %v", bb.Cmd)
			}
		})
	}
}

// A failed seed names the address it tried, so a typo or an unreachable
// host is obvious from the error alone.
func TestSeedErrorsNameTheSourceAddress(t *testing.T) {
	t.Setenv(SSLModeEnv, "")
	d := &recordingDriver{helperErr: errors.New("helper exited 1: timeout expired")}
	d.failFrom = 1 // the chown prep succeeds, the connection fails
	s := validSpec()
	s.Host, s.Port = "10.255.255.1", 6543
	err := Seed(context.Background(), d, s)
	if err == nil || !strings.Contains(err.Error(), "10.255.255.1:6543") || !strings.Contains(err.Error(), "timeout expired") {
		t.Fatalf("basebackup err = %v, want host:port and the helper output", err)
	}
	d = &recordingDriver{helperErr: errors.New("helper exited 1: timeout expired"), failFrom: 1}
	err = SeedDump(context.Background(), d, SeedDumpSpec{SeedSpec: s})
	if err == nil || !strings.Contains(err.Error(), "10.255.255.1:6543") {
		t.Fatalf("dump err = %v, want host:port", err)
	}
}
