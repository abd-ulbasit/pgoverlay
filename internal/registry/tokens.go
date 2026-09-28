package registry

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
)

// Roles, highest to lowest privilege. admin can do everything (incl. token and
// source management), operator can additionally mutate branches, viewer reads
// only. The order here is also the authz ranking the API enforces.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

// ValidRole reports whether role is one of the known roles.
func ValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleOperator, RoleViewer:
		return true
	}
	return false
}

// APIToken is a stored token's metadata (never the plaintext or its hash).
type APIToken struct {
	Name      string
	Role      string
	CreatedAt string
}

// hashToken returns the sha256 hex digest a token is stored and looked up by.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ErrInvalidTokenName rejects an API token name that could not be told apart
// from another identity in the audit log (see ValidateTokenName).
var ErrInvalidTokenName = errors.New("invalid token name")

// EnvTokenActor is the audit name recorded for the built-in PGOVERLAY_TOKEN,
// which is admin but has no stored name. It is reserved: a stored token with
// this name would be indistinguishable from the env token in the audit log.
const EnvTokenActor = "root"

// tokenNameRE is the shape of a stored token name. It excludes ':' (so names
// cannot collide with SystemActor or the local: prefix), spaces and parentheses
// (so the rendered "name (role)" actor is unambiguous), and the empty string
// (which renders as SystemActor).
var tokenNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidateTokenName checks a new API token name: 1-63 characters of lowercase
// letters, digits, '.', '_' and '-', starting with a letter or digit, and not
// the reserved env-token name "root". The error wraps ErrInvalidTokenName.
func ValidateTokenName(name string) error {
	if !tokenNameRE.MatchString(name) {
		return fmt.Errorf("%w %q: use 1-63 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", ErrInvalidTokenName, name)
	}
	if name == EnvTokenActor {
		return fmt.Errorf("%w %q: reserved for the built-in PGOVERLAY_TOKEN in the audit log", ErrInvalidTokenName, name)
	}
	return nil
}

// CreateAPIToken mints a token for the given name/role: it generates a 32-hex
// crypto/rand secret, stores only its sha256 hex digest, and returns the
// plaintext ONCE (it is never recoverable afterwards). The name must be unique
// and pass ValidateTokenName.
func (r *Registry) CreateAPIToken(name, role string) (string, error) {
	if err := ValidateTokenName(name); err != nil {
		return "", err
	}
	if !ValidRole(role) {
		return "", fmt.Errorf("invalid role %q: want admin, operator or viewer", role)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	plaintext := hex.EncodeToString(b) // 32 hex chars
	id := newID()
	if _, err := r.db.Exec(`INSERT INTO api_tokens (id,name,token_hash,role) VALUES (?,?,?,?)`,
		id, name, hashToken(plaintext), role); err != nil {
		return "", fmt.Errorf("create api token %q: %w", name, err)
	}
	return plaintext, nil
}

// LookupAPIToken resolves a presented plaintext token to its role via an
// indexed point lookup on token_hash (api_tokens_hash, v10). This is
// timing-safe without a constant-time compare: the discriminator is itself a
// SHA-256 of the secret, so a hit/miss reveals nothing about the plaintext and
// an attacker cannot steer the query toward a partial match. Returns ("",
// false) for the empty token, an unknown token, or any query error.
func (r *Registry) LookupAPIToken(plaintext string) (string, bool) {
	if plaintext == "" {
		return "", false
	}
	var role string
	err := r.db.QueryRow(`SELECT role FROM api_tokens WHERE token_hash=?`, hashToken(plaintext)).Scan(&role)
	if err != nil {
		return "", false // sql.ErrNoRows (unknown token) or any other error
	}
	return role, true
}

// LookupAPITokenActor resolves a presented plaintext token to its stored
// name and role via the same indexed point lookup as LookupAPIToken. The audit
// log uses the name to record WHO performed a branch mutation. Returns ("", "",
// false) for the empty token, an unknown token, or any query error.
func (r *Registry) LookupAPITokenActor(plaintext string) (name, role string, ok bool) {
	if plaintext == "" {
		return "", "", false
	}
	err := r.db.QueryRow(`SELECT name, role FROM api_tokens WHERE token_hash=?`, hashToken(plaintext)).Scan(&name, &role)
	if err != nil {
		return "", "", false
	}
	return name, role, true
}

// ListAPITokens returns token metadata (name/role/created_at) ordered by
// creation. It never returns the token plaintext or its hash.
func (r *Registry) ListAPITokens() ([]APIToken, error) {
	rows, err := r.db.Query(`SELECT name, role, created_at FROM api_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var tok APIToken
		if err := rows.Scan(&tok.Name, &tok.Role, &tok.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, tok)
	}
	return out, rows.Err()
}

// RevokeAPIToken deletes a token by name; ErrNotFound if no such token.
func (r *Registry) RevokeAPIToken(name string) error {
	res, err := r.db.Exec(`DELETE FROM api_tokens WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
