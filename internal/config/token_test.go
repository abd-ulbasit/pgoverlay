package config

import (
	"strings"
	"testing"
)

// SECRETS-05: the admin token has a length floor.
func TestValidateAdminToken(t *testing.T) {
	if err := ValidateAdminToken(""); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := ValidateAdminToken("devpass"); err == nil || !strings.Contains(err.Error(), "at least 16") {
		t.Fatalf("short token err=%v", err)
	}
	if err := ValidateAdminToken(strings.Repeat("x", MinAdminTokenLen-1)); err == nil {
		t.Fatal("15-char token accepted")
	}
	if err := ValidateAdminToken(strings.Repeat("x", MinAdminTokenLen)); err != nil {
		t.Fatalf("16-char token rejected: %v", err)
	}
}
