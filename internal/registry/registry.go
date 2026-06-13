package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

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

// ErrUnsupportedPGVersion rejects sources whose pg_version is outside the
// supported matrix. Majors 14-18 only: branch startup relies on
// recovery_init_sync_method=syncfs, which PG 13 and older do not have.
var ErrUnsupportedPGVersion = errors.New("unsupported pg_version")

// supportedPGVersions is the allowlist of Postgres majors pgbranch supports
// ("" defaults to the engine's image, postgres:17).
var supportedPGVersions = []string{"14", "15", "16", "17", "18"}

func validatePGVersion(v string) error {
	if v == "" {
		return nil
	}
	for _, s := range supportedPGVersions {
		if v == s {
			return nil
		}
	}
	return fmt.Errorf("%w %q: supported majors are %s (PG 13 and older lack recovery_init_sync_method=syncfs)",
		ErrUnsupportedPGVersion, v, strings.Join(supportedPGVersions, ", "))
}

// Seeding methods: how a source's data dir is built from the live Postgres.
const (
	SeedViaBasebackup = "basebackup" // pg_basebackup (needs REPLICATION privilege)
	SeedViaDump       = "dump"       // pg_dump into a fresh initdb'd cluster (managed Postgres)
)

type Source struct {
	ID, Name, PGVersion, Volume         string
	ConnHost, ConnUser, ConnDB, Network string
	SeedVia                             string   // SeedViaBasebackup (default) or SeedViaDump
	DumpSchemas                         []string // dump mode only: schemas to dump (empty = whole database)
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
	BaseLayerID                               string // top of the layer chain the branch bases on; "" = the source volume directly
	ParentBranchName                          string // display-only: branch this one was created from ("" = created from the source)
	Password                                  string // rotated per-branch password; "" = credentials inherited from the source
	Port                                      int
	State                                     BranchState
	CreatedAt                                 string
}

// Layer is a frozen branch rw volume: an immutable overlay layer between the
// source volume and the branches cloned from that branch. Layers chain via
// ParentLayerID ("" = the layer sits directly on the source volume).
type Layer struct {
	ID, SourceID, Volume, ParentLayerID string
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

// Ping verifies the registry's database handle is reachable (a trivial query).
// Used by branchd's /readyz check.
func (r *Registry) Ping(ctx context.Context) error { return r.db.PingContext(ctx) }

// CountBranchesByState returns the number of branches in each state (including
// 'destroyed' tombstones). Used by the metrics collector on scrape.
func (r *Registry) CountBranchesByState() (map[string]int, error) {
	return r.countByState(`SELECT state, count(*) FROM branches GROUP BY state`)
}

// CountSourcesByState returns the number of sources in each state. Used by the
// metrics collector on scrape.
func (r *Registry) CountSourcesByState() (map[string]int, error) {
	return r.countByState(`SELECT state, count(*) FROM sources GROUP BY state`)
}

func (r *Registry) countByState(query string) (map[string]int, error) {
	rows, err := r.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (r *Registry) CreateSource(s *Source) error {
	if err := validatePGVersion(s.PGVersion); err != nil {
		return err
	}
	if s.SeedVia == "" {
		s.SeedVia = SeedViaBasebackup
	}
	s.ID, s.State = newID(), SourceSeeding
	_, err := r.db.Exec(`INSERT INTO sources
		(id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,seed_via,dump_schemas,state)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Name, s.PGVersion, s.Volume, s.ConnHost, s.ConnPort, s.ConnUser, s.ConnDB, s.Network,
		s.SeedVia, strings.Join(s.DumpSchemas, ","), s.State)
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
	var dumpSchemas string
	err := row.Scan(&s.ID, &s.Name, &s.PGVersion, &s.Volume, &s.ConnHost, &s.ConnPort,
		&s.ConnUser, &s.ConnDB, &s.Network, &s.SeedVia, &dumpSchemas, &s.State, &s.Generation, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if dumpSchemas != "" {
		s.DumpSchemas = strings.Split(dumpSchemas, ",")
	}
	return s, err
}

const sourceCols = `id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,seed_via,dump_schemas,state,generation,created_at`

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

// nullable maps "" to SQL NULL (for nullable FK columns like parent_layer_id).
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (r *Registry) CreateBranch(b *Branch) error {
	b.ID, b.State = newID(), BranchCreating
	_, err := r.db.Exec(`INSERT INTO branches (id,name,source_id,state,rw_volume,source_volume,expires_at,base_layer_id,parent_branch_name) VALUES (?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Name, b.SourceID, b.State, b.RWVolume, b.SourceVolume, b.ExpiresAt, nullable(b.BaseLayerID), b.ParentBranchName)
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

// SetBranchPassword stores a branch's rotated per-branch password ("" =
// credentials inherited from the source). Called by the engine after the
// in-branch ALTER ROLE succeeded, before the branch is marked ready.
func (r *Registry) SetBranchPassword(id, password string) error {
	res, err := r.db.Exec(`UPDATE branches SET password=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, password, id)
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

func (r *Registry) MarkBranchReady(id, containerID, host string, port int) error {
	if _, err := r.db.Exec(`UPDATE branches SET container_id=?, host=?, port=? WHERE id=?`, containerID, host, port, id); err != nil {
		return err
	}
	return r.TransitionBranch(id, BranchReady, "instance running")
}

const branchCols = `id,name,source_id,state,container_id,rw_volume,source_volume,expires_at,host,base_layer_id,parent_branch_name,password,port,created_at`

func scanBranch(row interface{ Scan(...any) error }) (*Branch, error) {
	b := &Branch{}
	var baseLayer sql.NullString
	err := row.Scan(&b.ID, &b.Name, &b.SourceID, &b.State, &b.ContainerID, &b.RWVolume,
		&b.SourceVolume, &b.ExpiresAt, &b.Host, &baseLayer, &b.ParentBranchName, &b.Password, &b.Port, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	b.BaseLayerID = baseLayer.String
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

// ListStuckBranches returns branches still in a transient provisioning state
// (creating/resetting) whose last update predates the given deadline (RFC3339
// UTC, lexicographically comparable). A branch that has been creating/resetting
// longer than the stuck timeout is presumed abandoned (branchd died mid-saga)
// and reconcile fails it and cleans its resources. updated_at is the cutoff —
// a branch that legitimately takes a while to provision keeps bumping it.
func (r *Registry) ListStuckBranches(before string) ([]*Branch, error) {
	rows, err := r.db.Query(`SELECT `+branchCols+` FROM branches
		WHERE state IN ('creating','resetting') AND updated_at < ? ORDER BY created_at`, before)
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

// LiveVolumeSet returns the set of every volume name a live branch or a live
// source still depends on: every live branch's rw volume and source volume,
// every current source-generation volume, and every layer volume. Reconcile's
// volume GC keeps only volumes in this set; everything else carrying the
// pgbranch.managed label is an orphan. Computed in one snapshot so a volume
// can never be GC'd out from under a concurrently provisioning branch whose
// row was already committed.
func (r *Registry) LiveVolumeSet() (map[string]bool, error) {
	live := map[string]bool{}
	add := func(query string) error {
		rows, err := r.db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			if v != "" {
				live[v] = true
			}
		}
		return rows.Err()
	}
	if err := add(`SELECT rw_volume FROM branches WHERE state != 'destroyed'`); err != nil {
		return nil, err
	}
	if err := add(`SELECT source_volume FROM branches WHERE state != 'destroyed'`); err != nil {
		return nil, err
	}
	if err := add(`SELECT volume FROM sources`); err != nil {
		return nil, err
	}
	if err := add(`SELECT volume FROM layers`); err != nil {
		return nil, err
	}
	return live, nil
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

// CountLiveBranchesByRWVolume counts live branches whose writable layer is
// the given volume/dataset. Used as a GC guard: a volume that is some live
// branch's rw layer must never be removed as an orphaned source layer (zfs
// children record their parent's clone dataset as their SourceVolume).
func (r *Registry) CountLiveBranchesByRWVolume(volume string) (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT count(*) FROM branches WHERE rw_volume=? AND state!='destroyed'`, volume).Scan(&n)
	return n, err
}

// CreateLayer records a frozen layer (assigns its ID).
func (r *Registry) CreateLayer(l *Layer) error {
	l.ID = newID()
	_, err := r.db.Exec(`INSERT INTO layers (id,source_id,volume,parent_layer_id) VALUES (?,?,?,?)`,
		l.ID, l.SourceID, l.Volume, nullable(l.ParentLayerID))
	if err != nil {
		return fmt.Errorf("create layer for volume %q: %w", l.Volume, err)
	}
	return nil
}

func scanLayer(row interface{ Scan(...any) error }) (*Layer, error) {
	l := &Layer{}
	var parent sql.NullString
	err := row.Scan(&l.ID, &l.SourceID, &l.Volume, &parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	l.ParentLayerID = parent.String
	return l, err
}

const layerCols = `id,source_id,volume,parent_layer_id`

func (r *Registry) GetLayer(id string) (*Layer, error) {
	return scanLayer(r.db.QueryRow(`SELECT `+layerCols+` FROM layers WHERE id=?`, id))
}

// DeleteLayer removes a layer row. Fails (FK) while a child layer still
// chains onto it — callers GC topmost-first.
func (r *Registry) DeleteLayer(id string) error {
	res, err := r.db.Exec(`DELETE FROM layers WHERE id=?`, id)
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

// ListLayers returns every frozen layer across all sources. Reconcile walks
// it to GC layers whose refcount has dropped to zero.
func (r *Registry) ListLayers() ([]*Layer, error) {
	return r.listLayers(`SELECT ` + layerCols + ` FROM layers`)
}

// ListLayersBySource returns every layer frozen under the given source.
func (r *Registry) ListLayersBySource(sourceID string) ([]*Layer, error) {
	return r.listLayers(`SELECT `+layerCols+` FROM layers WHERE source_id=?`, sourceID)
}

func (r *Registry) listLayers(query string, args ...any) ([]*Layer, error) {
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Layer
	for rows.Next() {
		l, err := scanLayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LayerChain resolves a branch's layer chain, topmost (newest) layer first.
// Empty for branches based directly on the source volume. The full overlay
// stack is: chain[0], chain[1], …, source volume.
func (r *Registry) LayerChain(branchID string) ([]Layer, error) {
	b, err := r.getBranch(`id=?`, branchID)
	if err != nil {
		return nil, err
	}
	var out []Layer
	for id := b.BaseLayerID; id != ""; {
		l, err := r.GetLayer(id)
		if err != nil {
			return nil, fmt.Errorf("layer chain of branch %q: layer %q: %w", b.Name, id, err)
		}
		out = append(out, *l)
		id = l.ParentLayerID
	}
	return out, nil
}

// CountBranchesReferencingLayer computes a layer's refcount: the number of
// live branches whose layer chain contains it (directly or via descendants).
// Refcounts are derived, never stored.
func (r *Registry) CountBranchesReferencingLayer(layerID string) (int, error) {
	var n int
	err := r.db.QueryRow(`
		WITH RECURSIVE refs(branch_id, layer_id) AS (
			SELECT id, base_layer_id FROM branches
				WHERE state != 'destroyed' AND base_layer_id IS NOT NULL
			UNION
			SELECT refs.branch_id, layers.parent_layer_id FROM refs
				JOIN layers ON layers.id = refs.layer_id
				WHERE layers.parent_layer_id IS NOT NULL
		)
		SELECT count(DISTINCT branch_id) FROM refs WHERE layer_id = ?`, layerID).Scan(&n)
	return n, err
}

// CommitFreeze atomically records a completed freeze, once the parent is
// running on its fresh rw volume and the child instance is up:
//
//   - the parent's old rw volume becomes a new immutable layer (chained onto
//     the parent's previous base layer, if any),
//   - the parent row swaps to the fresh rw volume + new container/host/port
//     and transitions resetting -> ready,
//   - the child branch bases on the new layer.
//
// The parent must be mid-freeze (resetting); all of it commits or none does.
func (r *Registry) CommitFreeze(parentID, childID, layerVolume, newParentRW, containerID, host string, port int, reason string) (*Layer, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state, sourceID string
	var prevBase sql.NullString
	err = tx.QueryRow(`SELECT state, source_id, base_layer_id FROM branches WHERE id=?`, parentID).Scan(&state, &sourceID, &prevBase)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if BranchState(state) != BranchResetting {
		return nil, fmt.Errorf("illegal branch transition %s -> %s (freeze commit requires a resetting parent)", state, BranchReady)
	}
	l := &Layer{ID: newID(), SourceID: sourceID, Volume: layerVolume, ParentLayerID: prevBase.String}
	if _, err := tx.Exec(`INSERT INTO layers (id,source_id,volume,parent_layer_id) VALUES (?,?,?,?)`,
		l.ID, l.SourceID, l.Volume, nullable(l.ParentLayerID)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE branches SET rw_volume=?, base_layer_id=?, container_id=?, host=?, port=?, state=?,
		updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`,
		newParentRW, l.ID, containerID, host, port, BranchReady, parentID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason) VALUES (?,?,?,?,?)`,
		"branch", parentID, state, string(BranchReady), reason); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`UPDATE branches SET base_layer_id=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, l.ID, childID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, fmt.Errorf("freeze child branch: %w", ErrNotFound)
	}
	return l, tx.Commit()
}

// DeleteSource removes a source row and its (destroyed) branch history rows.
// Callers must ensure no live branches reference the source first.
func (r *Registry) DeleteSource(id string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// layers self-reference via parent_layer_id; defer FK checks so the whole
	// chain can go in one statement
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM layers WHERE source_id=?`, id); err != nil {
		return err
	}
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
