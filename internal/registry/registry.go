package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
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

// ErrAlreadyExists reports a create whose name is held by another live row.
// The error text names the row and its state; the API maps it to 409.
var ErrAlreadyExists = errors.New("already exists")

// ErrIllegalTransition reports a state change the state machine forbids (or
// a compare-and-swap that lost a race). Its text keeps the historical
// "illegal branch transition <from> -> <to>" form for branches.
var ErrIllegalTransition = errors.New("illegal transition")

// notFound names the missing row: `branch "x" not found`. It still matches
// errors.Is(err, ErrNotFound).
func notFound(kind, name string) error {
	return fmt.Errorf("%s %q %w", kind, name, ErrNotFound)
}

// illegalTransition renders the illegal-transition error for kind.
func illegalTransition(kind, from, to string) error {
	return &transitionError{kind: kind, from: from, to: to}
}

type transitionError struct{ kind, from, to string }

func (e *transitionError) Error() string {
	return fmt.Sprintf("illegal %s transition %s -> %s", e.kind, e.from, e.to)
}

func (e *transitionError) Is(target error) bool { return target == ErrIllegalTransition }

// isUniqueViolation reports a UNIQUE constraint failure (a live-name index).
func isUniqueViolation(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) && serr.Code() == sqlitelib.SQLITE_CONSTRAINT_UNIQUE
}

// ErrInvalidImage rejects a source image override that is not a plausible
// container image reference. The API maps it to 400.
var ErrInvalidImage = errors.New("invalid image reference")

// imageRefRe is a conservative image reference: optional registry host[:port],
// lowercase path components, optional :tag and @sha256 digest. It only has to
// keep garbage (spaces, shell or YAML metacharacters) out of the runtime.
var imageRefRe = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-]{1,2}[a-z0-9]+)*)*(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

func validateImage(image string) error {
	if image == "" {
		return nil
	}
	if len(image) > 255 || !imageRefRe.MatchString(image) {
		return fmt.Errorf("%w %q: want e.g. postgis/postgis:17-3.5 or ghcr.io/acme/postgres:17@sha256:…", ErrInvalidImage, image)
	}
	return nil
}

// ErrUnsupportedPGVersion rejects sources whose pg_version is outside the
// supported matrix. Majors 14-18 only: branch startup relies on
// recovery_init_sync_method=syncfs, which PG 13 and older do not have.
var ErrUnsupportedPGVersion = errors.New("unsupported pg_version")

// supportedPGVersions is the allowlist of Postgres majors pgoverlay supports
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

	// Image overrides the container image for the source's seed helpers and
	// every branch of it ("" = postgres:<PGVersion>). Branches run the
	// source's data directory, so the image must carry the same extensions,
	// locales and libc collation as the source server.
	Image string
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

	// PasswordUnavailable is set when the branch has a stored rotated password
	// that none of the configured secret keys can decrypt (the at-rest key
	// changed, or a legacy row was encrypted under an earlier PGOVERLAY_TOKEN).
	// Password is then "". The branch is otherwise fully usable: list, reset
	// (which mints and stores a fresh password) and destroy all work.
	PasswordUnavailable bool
}

// Layer is a frozen branch rw volume: an immutable overlay layer between the
// source volume and the branches cloned from that branch. Layers chain via
// ParentLayerID ("" = the layer sits directly on the source volume).
type Layer struct {
	ID, SourceID, Volume, ParentLayerID string
}

type Registry struct {
	db         *sql.DB
	instanceID string     // stable per-registry id; tags managed resources for GC scoping
	secrets    *secretBox // at-rest encryption for branch passwords; nil = plaintext (no key configured)
}

// SetSecretKey enables at-rest encryption of branch passwords under the given
// dedicated 32-byte key; it is SetSecretKeys with no legacy keys.
func (r *Registry) SetSecretKey(key []byte) error {
	return r.SetSecretKeys(SecretKeys{Primary: key})
}

// SetSecretKeys configures at-rest encryption of branch passwords. Call it once
// right after Open, before serving. No keys at all leaves the registry in
// plaintext mode (inherit-mode setups and tests need no key). A wrong-length
// key is a configuration error and is returned. With a primary key,
// SetBranchPassword encrypts before write; every read path decrypts with
// whichever configured key the row names, and legacy plaintext rows still read
// back unchanged. Call ReencryptSecrets afterwards to move older rows under
// the primary key.
func (r *Registry) SetSecretKeys(k SecretKeys) error {
	box, err := newSecretBox(k)
	if err != nil {
		return err
	}
	r.secrets = box
	return nil
}

// walRetryBudget bounds how long Open waits out the WAL-conversion race in
// connect; it mirrors the DSN's busy_timeout so a caller sees one timeout
// budget, not two.
const (
	walRetryBudget = 5 * time.Second
	walRetryDelay  = 20 * time.Millisecond
)

// dsnParams configures every registry connection. _txlock=immediate makes
// database/sql's BEGIN a BEGIN IMMEDIATE: the read-then-write transactions
// (TransitionBranch's compare-and-swap, CommitFreeze) take the write lock up
// front and wait under busy_timeout. A deferred BEGIN starts read-only, and in
// WAL mode upgrading it after another process (a second HA replica, or local
// pgb next to branchd) committed fails at once with SQLITE_BUSY_SNAPSHOT —
// the busy handler is never consulted for that upgrade.
const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"

func Open(path string) (*Registry, error) {
	db, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; keep it simple
	if err := connect(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open registry: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	id, err := ensureInstanceID(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("instance id: %w", err)
	}
	return &Registry{db: db, instanceID: id}, nil
}

// ensureInstanceID returns the registry's stable instance id, minting one (a
// 16-hex crypto/rand value) on first Open and persisting it in meta. Idempotent:
// the INSERT OR IGNORE keeps the first id even under a concurrent open of the
// same file, and subsequent opens read the stored value back.
func ensureInstanceID(db *sql.DB) (string, error) {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('instance_id', ?)`, hex.EncodeToString(id)); err != nil {
		return "", err
	}
	var stored string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='instance_id'`).Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}

// InstanceID returns this registry's stable instance id. The engine stamps it
// onto every managed Docker/K8s resource (label pgoverlay.instance) so reconcile
// reclaims only resources owned by this registry.
func (r *Registry) InstanceID() string { return r.instanceID }

// connect establishes the pool's first connection, retrying SQLITE_BUSY.
//
// Every connection applies journal_mode=WAL from the DSN, and converting a
// brand-new rollback-journal file to WAL needs a momentary EXCLUSIVE lock.
// SQLite deliberately does NOT invoke the busy handler for a SHARED->EXCLUSIVE
// upgrade — two upgraders each waiting on the other would deadlock, so it
// returns SQLITE_BUSY immediately — which means busy_timeout does not cover
// this one step. Two branchd replicas starting together on a fresh state dir
// could therefore both fail to open a database that is perfectly healthy.
//
// Retrying is the whole fix: WAL is a persistent property of the file, so the
// moment any opener wins the race, every other opener's journal_mode pragma
// becomes a no-op and succeeds. Only SQLITE_BUSY is retried; a genuinely
// unusable file still fails fast.
func connect(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(walRetryBudget)
	for {
		err := db.PingContext(ctx)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(walRetryDelay):
		}
	}
}

// isBusy reports whether err is SQLITE_BUSY — the lock is held elsewhere and
// the operation is worth retrying — as opposed to a real fault in the file.
func isBusy(err error) bool {
	var serr *sqlite.Error
	return errors.As(err, &serr) && serr.Code() == sqlitelib.SQLITE_BUSY
}

// ErrSchemaTooNew reports a registry written by a newer pgoverlay than this
// one. Migrations only go forward, so there is nothing safe to do: proceeding
// would run this build's queries against a schema it does not know.
var ErrSchemaTooNew = errors.New("registry schema is newer than this build")

// migrate applies pending versioned migrations (PRAGMA user_version).
//
// Every step re-reads user_version inside the same write transaction that
// applies the DDL and bumps it, so the version driving a migration is never a
// snapshot taken earlier and can never be written backwards. That matters
// because migrations are deliberately not replay-safe — SQLite has no ALTER
// TABLE ADD COLUMN IF NOT EXISTS, so a replayed v2 dies on "duplicate column
// name: expires_at" — and because schemaV1 *is* replay-safe, which is what made
// the old TOCTOU silent: an opener holding a stale version=0 sailed through
// migrations[0] and then committed user_version=1 over an already-current
// database, wedging every later open permanently. Reading the version under
// the transaction's write lock is what makes each migration apply exactly once,
// even when several branchd replicas open one registry file at the same time
// (branchd migrates at startup, before leader election, so they all do).
//
// Foreign keys are disabled for the duration because v2 recreates the sources
// table (drop + rename) while branches rows still reference it. The pragma is
// per-connection and a silent no-op inside a transaction, hence the pinned
// connection and the placement outside the loop.
func migrate(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx) // pin one conn: FK pragma is per-connection
	if err != nil {
		return err
	}
	defer conn.Close()
	// Fast path: an already-migrated registry takes no write lock at all, so
	// the common case (a replica restarting onto current state) never
	// contends. Safe now that the version cannot move backwards.
	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if err := checkVersion(version); err != nil {
		return err
	}
	if version == len(migrations) {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	for {
		done, err := migrateStep(ctx, conn)
		if err != nil || done {
			return err
		}
	}
}

// checkVersion rejects a user_version this build cannot reason about: negative
// (corrupt header) or ahead of the migrations it ships.
func checkVersion(version int) error {
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("%w: file is at v%d, this build knows up to v%d",
			ErrSchemaTooNew, version, len(migrations))
	}
	return nil
}

// migrateStep applies at most one migration and reports whether the schema is
// now fully migrated.
//
// BEGIN IMMEDIATE (rather than database/sql's plain BEGIN) is load-bearing: it
// takes the write lock up front, so the user_version read below is serialized
// against every other migrator. A deferred transaction starts read-only, which
// would let two processes read the same version before either wrote — exactly
// the race this function exists to close. Concurrent openers either block here
// until busy_timeout and then observe the committed version, or win the lock
// and are themselves observed.
func migrateStep(ctx context.Context, conn *sql.Conn) (done bool, err error) {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return false, err
	}
	if err := checkVersion(version); err != nil {
		return false, err
	}
	if version == len(migrations) {
		return true, nil
	}
	if _, err := conn.ExecContext(ctx, migrations[version]); err != nil {
		return false, fmt.Errorf("migrate to v%d: %w", version+1, err)
	}
	// Derived from the version read inside this transaction, so it is a
	// compare-and-set in effect: it can only ever move the file forward by one.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, version+1)); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, err
	}
	committed = true
	return false, nil
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

// CreateSource inserts a new source row (state seeding) and journals its
// creation in the same transaction, with the system actor; CreateSourceCtx
// records the request actor. A name whose earlier attempts failed is
// reusable: those failed rows are deleted here, so retries never pile up
// same-named failed rows (a failed source never has branches or layers — a
// branch needs a ready source). A name held by a live (seeding/ready) source
// is refused with ErrAlreadyExists.
func (r *Registry) CreateSource(s *Source) error {
	return r.CreateSourceCtx(context.Background(), s)
}

// CreateSourceCtx is CreateSource with the actor read from ctx (see WithActor)
// stamped onto the "created" transition.
func (r *Registry) CreateSourceCtx(ctx context.Context, s *Source) error {
	if err := validatePGVersion(s.PGVersion); err != nil {
		return err
	}
	if err := validateImage(s.Image); err != nil {
		return err
	}
	if s.SeedVia == "" {
		s.SeedVia = SeedViaBasebackup
	}
	s.ID, s.State = newID(), SourceSeeding
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := deleteFailedSources(tx, s.Name); err != nil {
		return fmt.Errorf("create source %q: %w", s.Name, err)
	}
	_, err = tx.Exec(`INSERT INTO sources
		(id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,seed_via,dump_schemas,state,image)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Name, s.PGVersion, s.Volume, s.ConnHost, s.ConnPort, s.ConnUser, s.ConnDB, s.Network,
		s.SeedVia, strings.Join(s.DumpSchemas, ","), s.State, s.Image)
	if isUniqueViolation(err) {
		var state string
		if qerr := tx.QueryRow(`SELECT state FROM sources WHERE name=? AND state!='failed'`, s.Name).Scan(&state); qerr != nil {
			state = "unknown"
		}
		return fmt.Errorf("source %q %w (state %s): remove it first or choose another name", s.Name, ErrAlreadyExists, state)
	}
	if err != nil {
		return fmt.Errorf("create source %q: %w", s.Name, err)
	}
	if err := journalTx(ctx, tx, "source", s.ID, "", string(SourceSeeding), "created"); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFailedSources removes every failed source row named name (and their
// masking scripts) and returns how many went. Failed rows own no branches,
// layers or volumes: the failed seed already removed its layer, and the
// volume name may now belong to a live row of the same name — so callers must
// never remove volumes on their behalf.
func (r *Registry) DeleteFailedSources(name string) (int, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := deleteFailedSources(tx, name)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func deleteFailedSources(tx *sql.Tx, name string) (int, error) {
	if _, err := tx.Exec(`DELETE FROM mask_scripts WHERE source_id IN
		(SELECT id FROM sources WHERE name=? AND state='failed')`, name); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM sources WHERE name=? AND state='failed'`, name)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// legalSource is the source state machine. A source is seeded exactly once:
// seeding ends ready or failed, and a failed seed is terminal (a retry
// creates a fresh row, see CreateSource). Refresh re-seeds into a new
// generation volume without leaving ready.
var legalSource = map[SourceState][]SourceState{
	SourceSeeding: {SourceReady, SourceFailed},
}

// SetSourceState moves a source into `to` if the state machine allows it,
// as a compare-and-swap with the journal row written in the same transaction
// (mirroring TransitionBranch), journaled with the system actor. Use
// SetSourceStateCtx to record the request actor.
func (r *Registry) SetSourceState(id string, to SourceState, reason string) error {
	return r.SetSourceStateCtx(context.Background(), id, to, reason)
}

// SetSourceStateCtx is SetSourceState with the actor read from ctx recorded on
// the transition.
func (r *Registry) SetSourceStateCtx(ctx context.Context, id string, to SourceState, reason string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from string
	if err := tx.QueryRow(`SELECT state FROM sources WHERE id=?`, id).Scan(&from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !slices.Contains(legalSource[SourceState(from)], to) {
		return illegalTransition("source", from, string(to))
	}
	res, err := tx.Exec(`UPDATE sources SET state=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id=? AND state=?`, string(to), id, from)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return illegalTransition("source", from, string(to))
	}
	if err := journalTx(ctx, tx, "source", id, from, string(to), reason); err != nil {
		return err
	}
	return tx.Commit()
}

// journalTx writes one transitions row inside tx, with the actor from ctx.
func journalTx(ctx context.Context, tx *sql.Tx, entity, id, from, to, reason string) error {
	_, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor) VALUES (?,?,?,?,?,?)`,
		entity, id, from, to, reason, actorString(ctx))
	return err
}

func scanSource(row interface{ Scan(...any) error }) (*Source, error) {
	s := &Source{}
	var dumpSchemas string
	err := row.Scan(&s.ID, &s.Name, &s.PGVersion, &s.Volume, &s.ConnHost, &s.ConnPort,
		&s.ConnUser, &s.ConnDB, &s.Network, &s.SeedVia, &dumpSchemas, &s.State, &s.Generation, &s.CreatedAt, &s.Image)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if dumpSchemas != "" {
		s.DumpSchemas = strings.Split(dumpSchemas, ",")
	}
	return s, err
}

const sourceCols = `id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,seed_via,dump_schemas,state,generation,created_at,image`

func (r *Registry) GetSourceByName(name string) (*Source, error) {
	// failed rows may share a name with a live retry; prefer the live one
	s, err := scanSource(r.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE name=?
		ORDER BY (state='failed') ASC, created_at DESC LIMIT 1`, name))
	if errors.Is(err, ErrNotFound) {
		return nil, notFound("source", name)
	}
	return s, err
}

func (r *Registry) GetSourceByID(id string) (*Source, error) {
	return scanSource(r.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
}

// legalBranch is the branch state machine.
//
//	creating   -> ready | failed
//	ready      -> resetting (reset, freeze/clone quiesce) | destroying
//	resetting  -> ready | failed
//	failed     -> resetting (reset, or recover onto the existing data) | destroying
//	destroying -> destroyed
//
// failed -> resetting keeps a failed branch recoverable: a branch failed by
// crash recovery (a freeze parent interrupted mid-freeze keeps its data) can
// be restarted on that data or reset, instead of only being destroyed.
// destroying has no way back, but DestroyBranch re-runs its idempotent
// teardown on a row already in destroying, so a failed destroy is retried
// rather than wedged.
var legalBranch = map[BranchState][]BranchState{
	BranchCreating:   {BranchReady, BranchFailed},
	BranchReady:      {BranchDestroying, BranchResetting},
	BranchResetting:  {BranchReady, BranchFailed},
	BranchFailed:     {BranchResetting, BranchDestroying},
	BranchDestroying: {BranchDestroyed},
}

// nullable maps "" to SQL NULL (for nullable FK columns like parent_layer_id).
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateBranch inserts a new branch row (state creating) and journals the
// initial transition with the system actor. Use CreateBranchCtx to record the
// request actor that initiated the create.
func (r *Registry) CreateBranch(b *Branch) error {
	return r.CreateBranchCtx(context.Background(), b)
}

// CreateBranchCtx is CreateBranch with the actor read from ctx (see WithActor)
// stamped onto the initial "created" transition.
//
// The row and its journal entry commit together. A name held by a live
// (non-destroyed) branch is refused with ErrAlreadyExists, naming the holder's
// state so a failed leftover is recognisable.
func (r *Registry) CreateBranchCtx(ctx context.Context, b *Branch) error {
	b.ID, b.State = newID(), BranchCreating
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO branches (id,name,source_id,state,rw_volume,source_volume,expires_at,base_layer_id,parent_branch_name) VALUES (?,?,?,?,?,?,?,?,?)`,
		b.ID, b.Name, b.SourceID, b.State, b.RWVolume, b.SourceVolume, b.ExpiresAt, nullable(b.BaseLayerID), b.ParentBranchName)
	if isUniqueViolation(err) {
		var state string
		if qerr := tx.QueryRow(`SELECT state FROM branches WHERE name=? AND state!='destroyed'`, b.Name).Scan(&state); qerr != nil {
			state = "unknown"
		}
		return fmt.Errorf("branch %q %w (state %s): destroy it first or choose another name", b.Name, ErrAlreadyExists, state)
	}
	if err != nil {
		return fmt.Errorf("create branch %q: %w", b.Name, err)
	}
	if err := journalTx(ctx, tx, "branch", b.ID, "", string(BranchCreating), "created"); err != nil {
		return err
	}
	return tx.Commit()
}

// TransitionBranch atomically moves a branch into `to`, but only from a legal
// source state. It is a compare-and-swap: a single conditional UPDATE guarded
// by `WHERE id=? AND state IN (<legal from-states>)`, with the transitions
// journal row written in the SAME transaction. This closes the TOCTOU window
// the old read-check-write had — two racing transitions can no longer both
// observe the same start state and both succeed; exactly one wins.
//
// Mirrors CommitFreeze: read the current state inside the tx (for an accurate
// journal from_state and the not-found/illegal distinction), then apply a
// state-guarded conditional UPDATE so the swap can never lose a concurrent race.
func (r *Registry) TransitionBranch(id string, to BranchState, reason string) error {
	return r.TransitionBranchCtx(context.Background(), id, to, reason)
}

// TransitionBranchCtx is TransitionBranch with the actor read from ctx (see
// WithActor) recorded in the transitions journal's actor column. The engine
// threads the request context here so create/destroy/reset record WHO did it;
// daemon-initiated transitions (reconcile, GC) pass a context with no actor and
// record SystemActor.
func (r *Registry) TransitionBranchCtx(ctx context.Context, id string, to BranchState, reason string) error {
	return r.transitionBranch(ctx, id, to, reason, "")
}

// transitionBranch is the compare-and-swap behind TransitionBranchCtx and
// MarkBranchReadyCtx. set is an optional extra SET clause ("col=?, …") whose
// args precede the WHERE args; it is applied by the same guarded UPDATE, so a
// transition that loses the race leaves those columns untouched too.
func (r *Registry) transitionBranch(ctx context.Context, id string, to BranchState, reason, set string, setArgs ...any) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var from string
	if err := tx.QueryRow(`SELECT state FROM branches WHERE id=?`, id).Scan(&from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !legalBranchTransition(BranchState(from), to) {
		return illegalTransition("branch", from, string(to))
	}

	// Conditional UPDATE: the `state=?` guard makes this a compare-and-swap.
	// It only fires while the row is STILL in the from-state we read above; a
	// concurrent winner that moved the row out from under us makes
	// RowsAffected()==0, so we never clobber its transition. (Under
	// SetMaxOpenConns(1) and BEGIN IMMEDIATE the SELECT+UPDATE in this tx are
	// already serialized, but the guard keeps the CAS correct regardless.)
	if set != "" {
		set += ", "
	}
	args := append(append([]any{}, setArgs...), string(to), id, from)
	res, err := tx.Exec(`UPDATE branches SET `+set+`state=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id=? AND state=?`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Lost the race: the row's state changed between our read and the
		// UPDATE. Re-read and report the same illegal-transition error a
		// from-state mismatch would have produced.
		var cur string
		if err := tx.QueryRow(`SELECT state FROM branches WHERE id=?`, id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return illegalTransition("branch", cur, string(to))
	}

	// CAS won: journal the transition in the same tx, with the exact prior
	// state and the actor from ctx.
	if err := journalTx(ctx, tx, "branch", id, from, string(to), reason); err != nil {
		return err
	}
	return tx.Commit()
}

// legalBranchTransition reports whether from -> to is permitted by legalBranch.
func legalBranchTransition(from, to BranchState) bool {
	for _, ok := range legalBranch[from] {
		if ok == to {
			return true
		}
	}
	return false
}

// SetBranchPassword stores a branch's rotated per-branch password ("" =
// credentials inherited from the source). Called by the engine after the
// in-branch ALTER ROLE succeeded, before the branch is marked ready.
func (r *Registry) SetBranchPassword(id, password string) error {
	// Encrypt at rest when a key is configured: the registry file lives on a
	// hostPath/PVC, so a plaintext password column hands every live branch's
	// working credential to anyone who can read the file. With no key set the
	// stored value is the plaintext (back-compat / inherit-mode / tests).
	stored, err := r.secrets.encrypt(password)
	if err != nil {
		return fmt.Errorf("encrypt branch password: %w", err)
	}
	res, err := r.db.Exec(`UPDATE branches SET password=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, stored, id)
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

// SetBranchContainer records a branch's container ID without changing its
// state. Provisioning calls this as soon as the container is started — before
// the readiness wait — so a concurrent reconcile sees the in-flight container
// as owned (in its "known" set) and does not reap it as an orphan.
//
// It also bumps updated_at: recording the in-flight container is saga progress,
// so a slow-but-alive create/freeze keeps resetting the stuck-timer (otherwise
// ListStuckBranches flags a legitimately slow op as abandoned — and a freeze
// parent's live data is then reaped). TouchBranch is the standalone bump for
// freeze checkpoints that don't change the container.
func (r *Registry) SetBranchContainer(id, containerID string) error {
	_, err := r.db.Exec(`UPDATE branches SET container_id=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, containerID, id)
	return err
}

// TouchBranch bumps a branch's updated_at without any other change: a saga
// progress checkpoint that resets the stuck-timer. Every saga's heartbeat
// (engine keepAlive) calls it periodically for the rows it keeps in
// creating/resetting, so a long-but-alive saga (slow readiness, long masking)
// never looks abandoned to ListStuckBranches.
func (r *Registry) TouchBranch(id string) error {
	_, err := r.db.Exec(`UPDATE branches SET updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, id)
	return err
}

func (r *Registry) MarkBranchReady(id, containerID, host string, port int) error {
	return r.MarkBranchReadyCtx(context.Background(), id, containerID, host, port)
}

// MarkBranchReadyCtx is MarkBranchReady with the actor read from ctx recorded
// on the creating/resetting -> ready transition.
//
// The container/host/port columns are written by the same compare-and-swap as
// the state, so a branch that was failed or destroyed concurrently never
// advertises a container its failing path did not see.
func (r *Registry) MarkBranchReadyCtx(ctx context.Context, id, containerID, host string, port int) error {
	return r.transitionBranch(ctx, id, BranchReady, "instance running",
		"container_id=?, host=?, port=?", containerID, host, port)
}

const branchCols = `id,name,source_id,state,container_id,rw_volume,source_volume,expires_at,host,base_layer_id,parent_branch_name,password,port,created_at`

// scanBranch reads a branch row, decrypting the password column on the way out
// so callers (API, engine) always see plaintext. It is a *Registry method
// because decryption needs the registry's secret key; the stored value carries
// the enc: prefix iff it was encrypted, so legacy plaintext rows pass through.
//
// A password no configured key can decrypt does NOT fail the read: every list,
// reconcile, reset and destroy path goes through here and none of them needs
// the plaintext, so one such row must not take them all down. The branch comes
// back with Password "" and PasswordUnavailable set; a reset re-mints it.
func (r *Registry) scanBranch(row interface{ Scan(...any) error }) (*Branch, error) {
	b := &Branch{}
	var baseLayer sql.NullString
	var storedPassword string
	err := row.Scan(&b.ID, &b.Name, &b.SourceID, &b.State, &b.ContainerID, &b.RWVolume,
		&b.SourceVolume, &b.ExpiresAt, &b.Host, &baseLayer, &b.ParentBranchName, &storedPassword, &b.Port, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.BaseLayerID = baseLayer.String
	if pw, derr := r.secrets.decrypt(storedPassword); derr != nil {
		b.PasswordUnavailable = true
	} else {
		b.Password = pw
	}
	return b, nil
}

func (r *Registry) getBranch(where string, args ...any) (*Branch, error) {
	return r.scanBranch(r.db.QueryRow(`SELECT `+branchCols+` FROM branches WHERE `+where, args...))
}

// GetBranchByName returns the live (non-destroyed) branch named name, or an
// error matching ErrNotFound that names it.
func (r *Registry) GetBranchByName(name string) (*Branch, error) {
	b, err := r.getBranch(`name=? AND state!='destroyed'`, name)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound("branch", name)
	}
	return b, err
}

// GetBranchByID returns a branch row by id, destroyed tombstones included.
func (r *Registry) GetBranchByID(id string) (*Branch, error) {
	return r.getBranch(`id=?`, id)
}

// NoteBranchCtx journals an event on a branch without changing its state (a
// transitions row with from_state = to_state = the current state) and bumps
// updated_at. DestroyBranch uses it to record why a teardown attempt failed,
// so `pgb history` shows the cause, and so a periodic retry backs off by one
// stuck-timeout per failed attempt.
func (r *Registry) NoteBranchCtx(ctx context.Context, id, reason string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRow(`SELECT state FROM branches WHERE id=?`, id).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE branches SET updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, id); err != nil {
		return err
	}
	if err := journalTx(ctx, tx, "branch", id, state, state, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) ListLiveBranches() ([]*Branch, error) {
	return r.listBranches(`state!='destroyed'`)
}

// Transition is one row of the branch audit log: a recorded state change, the
// reason, the actor (token name + role, the env-token sentinel, or SystemActor
// for daemon-initiated changes), and when it happened.
type Transition struct {
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	Reason    string `json:"reason"`
	Actor     string `json:"actor"`
	At        string `json:"at"`
}

// BranchHistory returns the audit trail for every branch that has ever borne
// the given name, oldest first. It matches transitions to the branch rows by
// id (a destroyed-then-recreated name maps to multiple ids), so an incident on
// a since-recreated name is still recoverable, and also by the entity_name
// DeleteSource stamps on the rows of branches it removes, so the trail
// outlives the source. ErrNotFound when the name was never used.
func (r *Registry) BranchHistory(name string) ([]Transition, error) {
	rows, err := r.db.Query(`SELECT t.from_state, t.to_state, t.reason, t.actor, t.at
		FROM transitions t
		WHERE t.entity = 'branch'
		  AND (t.entity_id IN (SELECT id FROM branches WHERE name = ?)
		       OR (t.entity_name != '' AND t.entity_name = ?))
		ORDER BY t.id ASC`, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.FromState, &t.ToState, &t.Reason, &t.Actor, &t.At); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, notFound("branch", name)
	}
	return out, nil
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
		WHERE state IN ('creating','resetting') AND updated_at < ? ORDER BY created_at`, msCutoff(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Branch
	for rows.Next() {
		b, err := r.scanBranch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// msCutoff renders an RFC3339 cutoff in the millisecond form updated_at is
// stored in (strftime '%Y-%m-%dT%H:%M:%fZ'). The two are compared as text,
// and a whole-second cutoff "…T15:04:05Z" sorts after every timestamp inside
// that second ('.' < 'Z'), so rows touched up to a second after the cutoff
// counted as older than it. Unparseable input is passed through unchanged.
func msCutoff(before string) string {
	t, err := time.Parse(time.RFC3339Nano, before)
	if err != nil {
		return before
	}
	return TimeString(t)
}

// VolumeNameUsed reports whether any registry row has ever used volume: as a
// branch's writable or source volume (destroyed tombstones included), as a
// frozen layer, or as a source generation. Branch creation picks writable
// volume names this reports unused, so a recreated branch name can never
// adopt a volume that still holds another branch's data — above all a frozen
// layer that live children mount read-only.
func (r *Registry) VolumeNameUsed(volume string) (bool, error) {
	var used bool
	err := r.db.QueryRow(volumeNameUsedQuery, volume).Scan(&used)
	return used, err
}

const volumeNameUsedQuery = `SELECT
	EXISTS (SELECT 1 FROM branches WHERE rw_volume=?1)
	OR EXISTS (SELECT 1 FROM branches WHERE source_volume=?1)
	OR EXISTS (SELECT 1 FROM branches WHERE pending_volume=?1)
	OR EXISTS (SELECT 1 FROM layers WHERE volume=?1)
	OR EXISTS (SELECT 1 FROM sources WHERE volume=?1)
	OR EXISTS (SELECT 1 FROM sources WHERE pending_volume=?1)`

// LiveVolumeSet returns the set of every volume name a live branch or a live
// source still depends on: every live branch's rw volume and source volume,
// every current source-generation volume, and every layer volume. Reconcile's
// volume GC keeps only volumes in this set; everything else carrying the
// pgoverlay.managed label is an orphan. Computed in one snapshot so a volume
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
	// volumes an in-flight saga created before any row names them: a freeze
	// parent's swap volume (claimed while the parent is mid-freeze; a crash
	// leaves the row failed and the claim, now dead, releases the volume to
	// GC) and a refresh's next generation
	if err := add(`SELECT pending_volume FROM branches WHERE state IN ('creating','resetting')`); err != nil {
		return nil, err
	}
	if err := add(`SELECT pending_volume FROM sources`); err != nil {
		return nil, err
	}
	return live, nil
}

// SetBranchPendingVolume records (volume != "") or clears (volume == "") the
// volume an in-flight saga is creating for a branch before any column names
// it — the freeze parent's swap volume. While the branch is creating or
// resetting, LiveVolumeSet counts it, so reconcile's volume GC leaves it
// alone. CommitFreeze clears it.
func (r *Registry) SetBranchPendingVolume(id, volume string) error {
	return r.setPending(`UPDATE branches SET pending_volume=? WHERE id=?`, id, volume)
}

// SetSourcePendingVolume is SetBranchPendingVolume for a source refresh's
// next-generation volume. BumpSourceGeneration clears it.
func (r *Registry) SetSourcePendingVolume(id, volume string) error {
	return r.setPending(`UPDATE sources SET pending_volume=? WHERE id=?`, id, volume)
}

func (r *Registry) setPending(query, id, volume string) error {
	res, err := r.db.Exec(query, volume, id)
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

func (r *Registry) listBranches(where string, args ...any) ([]*Branch, error) {
	rows, err := r.db.Query(`SELECT `+branchCols+` FROM branches WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Branch
	for rows.Next() {
		b, err := r.scanBranch(rows)
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
// a successful refresh seed, journaled with the system actor. Use
// BumpSourceGenerationCtx to record the request actor.
func (r *Registry) BumpSourceGeneration(id, newVolume string) error {
	return r.BumpSourceGenerationCtx(context.Background(), id, newVolume)
}

// BumpSourceGenerationCtx is BumpSourceGeneration with a transitions row
// (state unchanged, reason naming the new generation) recording the actor read
// from ctx, so a refresh, which changes what every new branch sees, is
// attributable like any other source mutation. It also clears the source's
// pending_volume claim on the new generation (see SetSourcePendingVolume).
func (r *Registry) BumpSourceGenerationCtx(ctx context.Context, id, newVolume string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE sources SET generation=generation+1, volume=?, pending_volume='',
		updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, newVolume, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	var state string
	var gen int
	if err := tx.QueryRow(`SELECT state, generation FROM sources WHERE id=?`, id).Scan(&state, &gen); err != nil {
		return err
	}
	if err := journalTx(ctx, tx, "source", id, state, state, fmt.Sprintf("refreshed to generation %d (%s)", gen, newVolume)); err != nil {
		return err
	}
	return tx.Commit()
}

// CountLiveBranches counts every branch that is not destroyed (creating,
// resetting, ready, failed and destroying all count). branchd's --max-branches
// quota compares this against its cap before provisioning a new branch.
func (r *Registry) CountLiveBranches() (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT count(*) FROM branches WHERE state!='destroyed'`).Scan(&n)
	return n, err
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

// CountLiveBranchesReferencingRW counts live branches (other than the named
// branch itself) that still depend on the given branch's writable volume — the
// guard the stuck-fail and destroy paths use to never delete a parent's data
// while a child needs it. A child references the parent's rw volume either
// directly, as its source_volume (csi/zfs clone the parent's PVC/dataset), or
// — in the overlay freeze, where the child's source_volume is the source and
// the parent's old rw volume only becomes a layer at CommitFreeze — by naming
// the parent in parent_branch_name while it has no base layer yet (the freeze
// has not committed). A committed overlay child bases on a frozen layer, not
// on the parent's current rw volume, so it does not count: the guard is
// precise in every state, including a destroy retried from destroying.
func (r *Registry) CountLiveBranchesReferencingRW(branchName, rwVolume string) (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT count(*) FROM branches
		WHERE state!='destroyed' AND name!=?
		AND (source_volume=? OR (parent_branch_name=? AND base_layer_id IS NULL))`,
		branchName, rwVolume, branchName).Scan(&n)
	return n, err
}

// InFlightChildren returns the names of branches still being created from the
// named branch (state creating, parent_branch_name = name). While one exists
// the parent's volumes may be mid-freeze or mid-clone, so reset and recover
// refuse to touch the parent.
func (r *Registry) InFlightChildren(name string) ([]string, error) {
	rows, err := r.db.Query(`SELECT name FROM branches
		WHERE state='creating' AND parent_branch_name=? ORDER BY created_at`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
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
	return r.CommitFreezeCtx(context.Background(), parentID, childID, layerVolume, newParentRW, containerID, host, port, reason)
}

// CommitFreezeCtx is CommitFreeze with the actor read from ctx recorded on the
// parent's resetting -> ready freeze-commit transition.
func (r *Registry) CommitFreezeCtx(ctx context.Context, parentID, childID, layerVolume, newParentRW, containerID, host string, port int, reason string) (*Layer, error) {
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
		return nil, fmt.Errorf("%w (freeze commit requires a resetting parent)", illegalTransition("branch", state, string(BranchReady)))
	}
	l := &Layer{ID: newID(), SourceID: sourceID, Volume: layerVolume, ParentLayerID: prevBase.String}
	if _, err := tx.Exec(`INSERT INTO layers (id,source_id,volume,parent_layer_id) VALUES (?,?,?,?)`,
		l.ID, l.SourceID, l.Volume, nullable(l.ParentLayerID)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE branches SET rw_volume=?, base_layer_id=?, container_id=?, host=?, port=?, state=?,
		pending_volume='', updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`,
		newParentRW, l.ID, containerID, host, port, BranchReady, parentID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor) VALUES (?,?,?,?,?,?)`,
		"branch", parentID, state, string(BranchReady), reason, actorString(ctx)); err != nil {
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

// sourceDeleted is the to_state journaled when a source row is removed. It is
// a journal-only marker: no source row ever carries it.
const sourceDeleted = "deleted"

// DeleteSource removes a source row and its (destroyed) branch rows, journaled
// with the system actor. Use DeleteSourceCtx to record the request actor.
func (r *Registry) DeleteSource(id string) error {
	return r.DeleteSourceCtx(context.Background(), id)
}

// DeleteSourceCtx removes a source row, its layers, masking scripts and
// (destroyed) branch rows. Callers must ensure no live branches reference the
// source first. The audit trail survives: before the rows go, every
// transitions row of the source and of its branches is stamped with the
// entity's name (so BranchHistory still resolves it by name), and a
// "<state> -> deleted" row records who removed the source, all in one
// transaction.
func (r *Registry) DeleteSourceCtx(ctx context.Context, id string) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name, state string
	if err := tx.QueryRow(`SELECT name, state FROM sources WHERE id=?`, id).Scan(&name, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE transitions
		SET entity_name = (SELECT b.name FROM branches b WHERE b.id = transitions.entity_id)
		WHERE entity = 'branch' AND entity_id IN (SELECT id FROM branches WHERE source_id = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE transitions SET entity_name = ? WHERE entity = 'source' AND entity_id = ?`, name, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor,entity_name) VALUES (?,?,?,?,?,?,?)`,
		"source", id, state, sourceDeleted, "source removed", actorString(ctx), name); err != nil {
		return err
	}
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

// Reconcile support: registry -> runtime drift repair and stuck-row recovery.
//
// The `before` arguments below are compared with updated_at, which is stored
// as strftime('%Y-%m-%dT%H:%M:%fZ') (UTC, millisecond precision); format them
// with TimeString so the lexicographic comparison is exact. (Each query also
// normalises its cut-off through msCutoff, so an RFC3339 whole-second value
// compares correctly too.)

// TimeString renders t in the registry's timestamp format (UTC, milliseconds),
// for the `before` cut-offs taken by the stuck-row queries below.
func TimeString(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// UpdateBranchEndpoint re-points a ready branch at a container and address
// without a state change: reconcile's repair for a branch whose container
// came back on a different address or had to be recreated. It is a
// compare-and-swap on state='ready' AND container_id=fromContainerID, so it
// never overwrites a branch that a concurrent reset, freeze or destroy has
// taken over; updated reports whether the row changed.
func (r *Registry) UpdateBranchEndpoint(id, fromContainerID, containerID, host string, port int) (updated bool, err error) {
	res, err := r.db.Exec(`UPDATE branches SET container_id=?, host=?, port=?
		WHERE id=? AND state=? AND container_id=?`,
		containerID, host, port, id, string(BranchReady), fromContainerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FailReadyBranch marks a ready branch failed when reconcile could not bring
// its container back. The state machine has no direct ready -> failed edge,
// so the row takes the two legal edges ready -> resetting -> failed, both
// journaled, in ONE transaction: a crash can never leave it parked in
// resetting, where the stuck-row recovery would treat its writable layer as
// a half-built reset and delete it. Compare-and-swap on state='ready' AND
// container_id=containerID; failed reports whether the row changed.
func (r *Registry) FailReadyBranch(ctx context.Context, id, containerID, reason string) (failed bool, err error) {
	if !legalBranchTransition(BranchReady, BranchResetting) || !legalBranchTransition(BranchResetting, BranchFailed) {
		return false, illegalTransition("branch", string(BranchReady), string(BranchFailed))
	}
	tx, err := r.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE branches SET state=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id=? AND state=? AND container_id=?`, string(BranchFailed), id, string(BranchReady), containerID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	actor := actorString(ctx)
	for _, step := range [][2]BranchState{{BranchReady, BranchResetting}, {BranchResetting, BranchFailed}} {
		if _, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor) VALUES (?,?,?,?,?,?)`,
			"branch", id, string(step[0]), string(step[1]), reason, actor); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// FailStuckBranch moves a branch still in creating or resetting whose last
// update is older than before to failed, journaling reason — one
// compare-and-swap on the same criterion ListStuckBranches uses. It returns
// false and changes nothing when the row has moved on (its saga finished or
// failed) or made progress since, so reconcile tears down only a branch it
// actually failed.
func (r *Registry) FailStuckBranch(ctx context.Context, id, before, reason string) (failed bool, err error) {
	before = msCutoff(before)
	tx, err := r.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var from string
	if err := tx.QueryRow(`SELECT state FROM branches WHERE id=? AND state IN (?,?) AND updated_at < ?`,
		id, string(BranchCreating), string(BranchResetting), before).Scan(&from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !legalBranchTransition(BranchState(from), BranchFailed) {
		return false, illegalTransition("branch", from, string(BranchFailed))
	}
	res, err := tx.Exec(`UPDATE branches SET state=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id=? AND state=? AND updated_at < ?`, string(BranchFailed), id, from, before)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor) VALUES (?,?,?,?,?,?)`,
		"branch", id, from, string(BranchFailed), reason, actorString(ctx)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListStuckDestroyingBranches returns branches left in destroying — a destroy
// whose teardown failed or was interrupted (branchd crash, shutdown) — whose
// last update predates before (RFC3339 UTC). DestroyBranch accepts such a row
// and re-runs its idempotent teardown, so reconcile's retry_destroy can finish
// them; every failed attempt bumps updated_at (NoteBranchCtx), so the retries
// back off by the cutoff's age.
func (r *Registry) ListStuckDestroyingBranches(before string) ([]*Branch, error) {
	return r.listBranches(`state=? AND updated_at < ?`, string(BranchDestroying), msCutoff(before))
}

// TouchSource bumps a source's updated_at: the seeding heartbeat that tells
// reconcile the seed is still running (see ListStuckSources).
func (r *Registry) TouchSource(id string) error {
	_, err := r.db.Exec(`UPDATE sources SET updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, id)
	return err
}

// ListStuckSources returns sources still seeding whose last update is older
// than before. A live seed heartbeats (TouchSource), so these are seeds whose
// process died.
func (r *Registry) ListStuckSources(before string) ([]*Source, error) {
	rows, err := r.db.Query(`SELECT `+sourceCols+` FROM sources WHERE state=? AND updated_at < ? ORDER BY created_at`,
		string(SourceSeeding), msCutoff(before))
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

// FailStuckSource moves a source still seeding whose last update is older than
// before to failed, journaling reason, as one compare-and-swap; failed reports
// whether it applied (false when the seed finished, failed or heartbeat since).
func (r *Registry) FailStuckSource(ctx context.Context, id, before, reason string) (failed bool, err error) {
	tx, err := r.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE sources SET state=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id=? AND state=? AND updated_at < ?`, string(SourceFailed), id, string(SourceSeeding), msCutoff(before))
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO transitions (entity,entity_id,from_state,to_state,reason,actor) VALUES (?,?,?,?,?,?)`,
		"source", id, string(SourceSeeding), string(SourceFailed), reason, actorString(ctx)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
