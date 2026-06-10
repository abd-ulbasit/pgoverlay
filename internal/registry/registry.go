package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

type SourceState string
type BranchState string

const (
	SourceSeeding SourceState = "seeding"
	SourceReady   SourceState = "ready"
	SourceFailed  SourceState = "failed"

	BranchCreating   BranchState = "creating"
	BranchReady      BranchState = "ready"
	BranchFailed     BranchState = "failed"
	BranchResetting  BranchState = "resetting"
	BranchDestroying BranchState = "destroying"
	BranchDestroyed  BranchState = "destroyed"
)

var ErrNotFound = errors.New("not found")

type Source struct {
	ID, Name, PGVersion, Volume         string
	ConnHost, ConnUser, ConnDB, Network string
	ConnPort                            int
	Generation                          int
	State                               SourceState
	CreatedAt                           string
}

type Branch struct {
	ID, Name, SourceID, ContainerID, RWVolume string
	SourceVolume                              string // source volume the branch was created from
	ExpiresAt                                 string // RFC3339, "" = never
	Host                                      string // address the instance listens on (127.0.0.1 for docker, pod IP for k8s)
	Port                                      int
	State                                     BranchState
	CreatedAt                                 string
}

type Registry struct{ db *sql.DB }

func Open(path string) (*Registry, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; keep it simple
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Registry{db: db}, nil
}

// migrate applies pending versioned migrations (PRAGMA user_version). Each
// migration runs in its own transaction; foreign keys are disabled for the
// duration because v2 recreates the sources table (drop + rename) while
// branches rows still reference it.
func migrate(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx) // pin one conn: FK pragma is per-connection
	if err != nil {
		return err
	}
	defer conn.Close()
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= len(migrations) {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	for ; version < len(migrations); version++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate to v%d: %w", version+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, version+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) Close() error { return r.db.Close() }

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (r *Registry) CreateSource(s *Source) error {
	s.ID, s.State = newID(), SourceSeeding
	_, err := r.db.Exec(`INSERT INTO sources
		(id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,state)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Name, s.PGVersion, s.Volume, s.ConnHost, s.ConnPort, s.ConnUser, s.ConnDB, s.Network, s.State)
	if err != nil {
		return fmt.Errorf("create source %q: %w", s.Name, err)
	}
	return r.journal("source", s.ID, "", string(SourceSeeding), "created")
}

func (r *Registry) SetSourceState(id string, to SourceState, reason string) error {
	return r.setState("sources", "source", id, string(to), reason)
}

func (r *Registry) setState(table, entity, id, to, reason string) error {
	var from string
	if err := r.db.QueryRow(`SELECT state FROM `+table+` WHERE id=?`, id).Scan(&from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := r.db.Exec(`UPDATE `+table+` SET state=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, to, id); err != nil {
		return err
	}
	return r.journal(entity, id, from, to, reason)
}

func (r *Registry) journal(entity, id, from, to, reason string) error {
	_, err := r.db.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason) VALUES (?,?,?,?,?)`,
		entity, id, from, to, reason)
	return err
}

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	s := &Source{}
	err := row.Scan(&s.ID, &s.Name, &s.PGVersion, &s.Volume, &s.ConnHost, &s.ConnPort,
		&s.ConnUser, &s.ConnDB, &s.Network, &s.State, &s.Generation, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

const sourceCols = `id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,state,generation,created_at`

func (r *Registry) GetSourceByName(name string) (*Source, error) {
	// failed rows may share a name with a live retry; prefer the live one
	return scanSource(r.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE name=?
		ORDER BY (state='failed') ASC, created_at DESC LIMIT 1`, name))
}

func (r *Registry) GetSourceByID(id string) (*Source, error) {
	return scanSource(r.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
}

var legalBranch = map[BranchState][]BranchState{
	BranchCreating:   {BranchReady, BranchFailed},
	BranchReady:      {BranchDestroying, BranchResetting},
	BranchResetting:  {BranchReady, BranchFailed},
	BranchFailed:     {BranchDestroying},
	BranchDestroying: {BranchDestroyed},
}

func (r *Registry) CreateBranch(b *Branch) error {
	b.ID, b.State = newID(), BranchCreating
	_, err := r.db.Exec(`INSERT INTO branches (id,name,source_id,state,rw_volume,source_volume,expires_at) VALUES (?,?,?,?,?,?,?)`,
		b.ID, b.Name, b.SourceID, b.State, b.RWVolume, b.SourceVolume, b.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create branch %q: %w", b.Name, err)
	}
	return r.journal("branch", b.ID, "", string(BranchCreating), "created")
}

func (r *Registry) TransitionBranch(id string, to BranchState, reason string) error {
	b, err := r.getBranch(`id=?`, id)
	if err != nil {
		return err
	}
	for _, ok := range legalBranch[b.State] {
		if ok == to {
			return r.setState("branches", "branch", id, string(to), reason)
		}
	}
	return fmt.Errorf("illegal branch transition %s -> %s", b.State, to)
}

func (r *Registry) MarkBranchReady(id, containerID, host string, port int) error {
	if _, err := r.db.Exec(`UPDATE branches SET container_id=?, host=?, port=? WHERE id=?`, containerID, host, port, id); err != nil {
		return err
	}
	return r.TransitionBranch(id, BranchReady, "instance running")
}

const branchCols = `id,name,source_id,state,container_id,rw_volume,source_volume,expires_at,host,port,created_at`

func scanBranch(row interface{ Scan(...any) error }) (*Branch, error) {
	b := &Branch{}
	err := row.Scan(&b.ID, &b.Name, &b.SourceID, &b.State, &b.ContainerID, &b.RWVolume,
		&b.SourceVolume, &b.ExpiresAt, &b.Host, &b.Port, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (r *Registry) getBranch(where string, args ...any) (*Branch, error) {
	return scanBranch(r.db.QueryRow(`SELECT `+branchCols+` FROM branches WHERE `+where, args...))
}

func (r *Registry) GetBranchByName(name string) (*Branch, error) {
	return r.getBranch(`name=? AND state!='destroyed'`, name)
}

func (r *Registry) ListLiveBranches() ([]*Branch, error) {
	return r.listBranches(`state!='destroyed'`)
}

// ListExpiredBranches returns ready/failed branches whose expiry (RFC3339
// UTC, lexicographically comparable) has passed. now must be RFC3339 UTC.
func (r *Registry) ListExpiredBranches(now string) ([]*Branch, error) {
	return r.listBranches(`state IN ('ready','failed') AND expires_at != '' AND expires_at < ?`, now)
}

func (r *Registry) listBranches(where string, args ...any) ([]*Branch, error) {
	rows, err := r.db.Query(`SELECT `+branchCols+` FROM branches WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Branch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MaskScript is one masking statement: SQL the engine runs (via in-container
// psql) on every new/reset branch of the owning source, in stored order.
type MaskScript struct {
	Name string
	SQL  string
}

// SetMaskScripts replaces a source's masking scripts with the given ordered
// list (empty/nil clears them).
func (r *Registry) SetMaskScripts(sourceID string, scripts []MaskScript) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM mask_scripts WHERE source_id=?`, sourceID); err != nil {
		return err
	}
	for i, s := range scripts {
		if _, err := tx.Exec(`INSERT INTO mask_scripts (source_id,ord,name,sql) VALUES (?,?,?,?)`,
			sourceID, i, s.Name, s.SQL); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMaskScripts returns a source's masking scripts in application order.
func (r *Registry) GetMaskScripts(sourceID string) ([]MaskScript, error) {
	rows, err := r.db.Query(`SELECT name, sql FROM mask_scripts WHERE source_id=? ORDER BY ord`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaskScript
	for rows.Next() {
		var s MaskScript
		if err := rows.Scan(&s.Name, &s.SQL); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BumpSourceGeneration advances a source to its next generation volume after
// a successful refresh seed.
func (r *Registry) BumpSourceGeneration(id, newVolume string) error {
	res, err := r.db.Exec(`UPDATE sources SET generation=generation+1, volume=?,
		updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, newVolume, id)
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

func (r *Registry) CountLiveBranchesBySource(sourceID string) (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT count(*) FROM branches WHERE source_id=? AND state!='destroyed'`, sourceID).Scan(&n)
	return n, err
}

func (r *Registry) CountLiveBranchesByVolume(volume string) (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT count(*) FROM branches WHERE source_volume=? AND state!='destroyed'`, volume).Scan(&n)
	return n, err
}

// DeleteSource removes a source row and its (destroyed) branch history rows.
// Callers must ensure no live branches reference the source first.
func (r *Registry) DeleteSource(id string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM branches WHERE source_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mask_scripts WHERE source_id=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (r *Registry) ListSources() ([]*Source, error) {
	rows, err := r.db.Query(`SELECT ` + sourceCols + ` FROM sources ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
