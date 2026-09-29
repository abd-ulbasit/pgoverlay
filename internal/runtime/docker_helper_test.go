package runtime

import "testing"

// The copy-up probe mounts an overlay: a SysAdmin helper gets what a branch
// container gets for its overlay mount, CAP_SYS_ADMIN with AppArmor
// unconfined, and nothing more.
func TestHelperHostConfigSysAdmin(t *testing.T) {
	h := helperHostConfig(HelperSpec{SysAdmin: true})
	if h.Privileged || len(h.CapAdd) != 1 || h.CapAdd[0] != "SYS_ADMIN" || len(h.SecurityOpt) != 1 || h.SecurityOpt[0] != "apparmor=unconfined" {
		t.Fatalf("SysAdmin helper host config: priv=%v caps=%v secopt=%v", h.Privileged, h.CapAdd, h.SecurityOpt)
	}
	h = helperHostConfig(HelperSpec{SysAdmin: true, Privileged: true})
	if !h.Privileged || len(h.CapAdd) != 0 {
		t.Fatalf("Privileged wins: priv=%v caps=%v", h.Privileged, h.CapAdd)
	}
	h = helperHostConfig(HelperSpec{})
	if h.Privileged || len(h.CapAdd) != 0 || len(h.SecurityOpt) != 0 {
		t.Fatalf("plain helper: priv=%v caps=%v secopt=%v", h.Privileged, h.CapAdd, h.SecurityOpt)
	}
}
