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
