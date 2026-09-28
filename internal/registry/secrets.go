package registry

import "fmt"

// SecretsReport is the outcome of ReencryptSecrets.
type SecretsReport struct {
	// Reencrypted counts live branch passwords rewritten under the primary
	// key (legacy plaintext, enc:v1: rows, or enc:v2: rows under another key).
	Reencrypted int
	// Unavailable names the live branches whose stored password no configured
	// key can decrypt. They keep working (see Branch.PasswordUnavailable);
	// resetting them mints a new password.
	Unavailable []string
}

// ReencryptSecrets moves every live branch password under the primary at-rest
// key: plaintext rows written before encryption (or while no key was set),
// enc:v1: rows encrypted under sha256(PGOVERLAY_TOKEN), and enc:v2: rows under
// a previous key all get rewritten, so the registry file stops carrying
// passwords in a weaker or retired form. Rows no configured key can decrypt
// are left untouched and reported. branchd calls this once at startup, after
// SetSecretKeys. Without a primary key it does nothing.
//
// Each rewrite is a compare-and-swap on the stored value, so a password
// rotated concurrently (another replica, a reset in flight) is never clobbered
// with an older one.
func (r *Registry) ReencryptSecrets() (SecretsReport, error) {
	var rep SecretsReport
	if r.secrets == nil || r.secrets.primaryID == "" {
		return rep, nil
	}
	tx, err := r.db.Begin()
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()

	type row struct{ id, name, stored string }
	rows, err := tx.Query(`SELECT id, name, password FROM branches
		WHERE password != '' AND state != 'destroyed' ORDER BY created_at`)
	if err != nil {
		return rep, err
	}
	var todo []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.id, &rw.name, &rw.stored); err != nil {
			rows.Close()
			return rep, err
		}
		if !r.secrets.isCurrent(rw.stored) {
			todo = append(todo, rw)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	for _, rw := range todo {
		plain, err := r.secrets.decrypt(rw.stored)
		if err != nil {
			rep.Unavailable = append(rep.Unavailable, rw.name)
			continue
		}
		enc, err := r.secrets.encrypt(plain)
		if err != nil {
			return rep, fmt.Errorf("re-encrypt password of branch %q: %w", rw.name, err)
		}
		res, err := tx.Exec(`UPDATE branches SET password=? WHERE id=? AND password=?`, enc, rw.id, rw.stored)
		if err != nil {
			return rep, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return rep, err
		} else if n == 1 {
			rep.Reencrypted++
		}
	}
	return rep, tx.Commit()
}
