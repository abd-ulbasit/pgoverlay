package config

import (
	"errors"
	"fmt"
)

// MinAdminTokenLen is the shortest PGOVERLAY_TOKEN branchd accepts. The token
// is the built-in admin credential for the whole REST API, so a short or
// human-chosen value is refused at startup, the same floor ghook applies to its
// webhook secret. `openssl rand -hex 16` (32 characters) is the documented way
// to make one.
const MinAdminTokenLen = 16

// ValidateAdminToken checks the PGOVERLAY_TOKEN value branchd is started with.
func ValidateAdminToken(token string) error {
	if token == "" {
		return errors.New("PGOVERLAY_TOKEN must be set (bearer token for the REST API; generate one with: openssl rand -hex 16)")
	}
	if len(token) < MinAdminTokenLen {
		return fmt.Errorf("PGOVERLAY_TOKEN must be at least %d characters (it is the admin credential for the REST API); generate one with: openssl rand -hex 16", MinAdminTokenLen)
	}
	return nil
}
