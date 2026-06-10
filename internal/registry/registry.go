package registry

import (
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
	BranchDestroying BranchState = "destroying"
	BranchDestroyed  BranchState = "destroyed"
)

var ErrNotFound = errors.New("not found")

type Source struct {
	ID, Name, PGVersion, Volume         string
	ConnHost, ConnUser, ConnDB, Network string
	ConnPort                            int
	State                               SourceState
	CreatedAt                           string
}

type Branch struct {
	ID, Name, SourceID, ContainerID, RWVolume string
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
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Registry{db: db}, nil
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
		&s.ConnUser, &s.ConnDB, &s.Network, &s.State, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

const sourceCols = `id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,state,created_at`

func (r *Registry) GetSourceByName(name string) (*Source, error) {
	return scanSource(r.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE name=?`, name))
}

var legalBranch = map[BranchState][]BranchState{
	BranchCreating:   {BranchReady, BranchFailed},
	BranchReady:      {BranchDestroying},
	BranchFailed:     {BranchDestroying},
	BranchDestroying: {BranchDestroyed},
}

func (r *Registry) CreateBranch(b *Branch) error {
	b.ID, b.State = newID(), BranchCreating
	_, err := r.db.Exec(`INSERT INTO branches (id,name,source_id,state,rw_volume) VALUES (?,?,?,?,?)`,
		b.ID, b.Name, b.SourceID, b.State, b.RWVolume)
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

func (r *Registry) MarkBranchReady(id, containerID string, port int) error {
	if _, err := r.db.Exec(`UPDATE branches SET container_id=?, port=? WHERE id=?`, containerID, port, id); err != nil {
		return err
	}
	return r.TransitionBranch(id, BranchReady, "instance running")
}

const branchCols = `id,name,source_id,state,container_id,rw_volume,port,created_at`

func (r *Registry) getBranch(where string, args ...any) (*Branch, error) {
	b := &Branch{}
	err := r.db.QueryRow(`SELECT `+branchCols+` FROM branches WHERE `+where, args...).
		Scan(&b.ID, &b.Name, &b.SourceID, &b.State, &b.ContainerID, &b.RWVolume, &b.Port, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (r *Registry) GetBranchByName(name string) (*Branch, error) {
	return r.getBranch(`name=? AND state!='destroyed'`, name)
}

func (r *Registry) ListLiveBranches() ([]*Branch, error) {
	rows, err := r.db.Query(`SELECT ` + branchCols + ` FROM branches WHERE state!='destroyed' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Branch
	for rows.Next() {
		b := &Branch{}
		if err := rows.Scan(&b.ID, &b.Name, &b.SourceID, &b.State, &b.ContainerID, &b.RWVolume, &b.Port, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
