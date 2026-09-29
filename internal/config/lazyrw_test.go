package config

import "testing"

// PGOVERLAY_LAZYRW and PGOVERLAY_WAL_RECYCLE reach the config as given, and
// parse as on|off with an empty value meaning the default (on for both).
func TestLazyRWAndWALRecycleSettings(t *testing.T) {
	t.Setenv(LazyRWEnv, "")
	t.Setenv(WALRecycleEnv, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"LazyRW": c.LazyRW, "WALRecycle": c.WALRecycle} {
		if on, err := ParseOnOff(v, true); err != nil || !on {
			t.Errorf("%s = %q parses to %v (err %v), want on by default", name, v, on, err)
		}
	}
	t.Setenv(LazyRWEnv, "off")
	t.Setenv(WALRecycleEnv, "off")
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if c.LazyRW != "off" || c.WALRecycle != "off" {
		t.Fatalf("LazyRW = %q, WALRecycle = %q, want off from the environment", c.LazyRW, c.WALRecycle)
	}
	for in, want := range map[string]bool{"on": true, "off": false} {
		if got, err := ParseOnOff(in, !want); err != nil || got != want {
			t.Errorf("ParseOnOff(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"true", "0", "ON", "yes", " on"} {
		if _, err := ParseOnOff(bad, true); err == nil {
			t.Errorf("ParseOnOff(%q) accepted", bad)
		}
	}
}
