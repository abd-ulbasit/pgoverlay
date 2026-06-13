package registry

// Schema is versioned via PRAGMA user_version: migrations[i] upgrades a
// database at version i to version i+1. Phase 1 shipped with user_version 0
// and the v1 tables already created, so schemaV1 stays IF NOT EXISTS — it is
// a no-op on an existing P1 database and a full create on a fresh one.
var migrations = []string{schemaV1, migrateV2, migrateV3, migrateV4, migrateV5, migrateV6, migrateV7, migrateV8}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS sources (
  id TEXT PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  pg_version TEXT NOT NULL,
  volume TEXT NOT NULL,
  conn_host TEXT NOT NULL DEFAULT '',
  conn_port INTEGER NOT NULL DEFAULT 0,
  conn_user TEXT NOT NULL DEFAULT '',
  conn_db   TEXT NOT NULL DEFAULT '',
  network   TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS branches (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  source_id TEXT NOT NULL REFERENCES sources(id),
  state TEXT NOT NULL,
  container_id TEXT NOT NULL DEFAULT '',
  rw_volume TEXT NOT NULL,
  port INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
-- name unique among live branches only (destroyed rows kept for history)
CREATE UNIQUE INDEX IF NOT EXISTS branches_live_name
  ON branches(name) WHERE state != 'destroyed';
CREATE TABLE IF NOT EXISTS transitions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  entity TEXT NOT NULL,        -- 'source' | 'branch'
  entity_id TEXT NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
`

// v2 (Phase 2): branch TTLs + pinned source volumes, source generations, and
// the sources table is recreated to drop the table-level UNIQUE on name in
// favor of a partial unique index so failed seeds don't block retries.
const migrateV2 = `
ALTER TABLE branches ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE branches ADD COLUMN source_volume TEXT NOT NULL DEFAULT '';
UPDATE branches SET source_volume =
  COALESCE((SELECT volume FROM sources WHERE sources.id = branches.source_id), '')
  WHERE source_volume = '';
CREATE TABLE sources_new (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  pg_version TEXT NOT NULL,
  volume TEXT NOT NULL,
  conn_host TEXT NOT NULL DEFAULT '',
  conn_port INTEGER NOT NULL DEFAULT 0,
  conn_user TEXT NOT NULL DEFAULT '',
  conn_db   TEXT NOT NULL DEFAULT '',
  network   TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  generation INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO sources_new (id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,state,created_at,updated_at)
  SELECT id,name,pg_version,volume,conn_host,conn_port,conn_user,conn_db,network,state,created_at,updated_at FROM sources;
DROP TABLE sources;
ALTER TABLE sources_new RENAME TO sources;
-- name unique among non-failed sources only (failed seeds don't block retry)
CREATE UNIQUE INDEX sources_live_name ON sources(name) WHERE state != 'failed';
`

// v3 (Phase 3): branches record the host their instance listens on. Docker
// publishes on 127.0.0.1 (the historical behavior, hence the backfill
// default); the K8s driver stores the pod IP.
const migrateV3 = `
ALTER TABLE branches ADD COLUMN host TEXT NOT NULL DEFAULT '127.0.0.1';
`

// v4 (Phase 4): per-source ordered masking SQL, applied by the engine inside
// every branch create/reset before the branch is marked ready.
const migrateV4 = `
CREATE TABLE mask_scripts (
  source_id TEXT NOT NULL,
  ord INTEGER NOT NULL,
  name TEXT NOT NULL,
  sql TEXT NOT NULL,
  PRIMARY KEY (source_id, ord)
);
`

// v5 (Phase 5): branch-from-branch. A freeze turns a parent branch's rw
// volume into an immutable layer; layers form per-source chains (a DAG rooted
// at the source volume). branches.base_layer_id points at the top of the
// chain a branch was cloned from (NULL = directly off the source volume, as
// before); parent_branch_name is display-only lineage.
const migrateV5 = `
CREATE TABLE layers (
  id TEXT PRIMARY KEY,
  source_id TEXT NOT NULL,
  volume TEXT NOT NULL,
  parent_layer_id TEXT NULL REFERENCES layers(id)
);
ALTER TABLE branches ADD COLUMN base_layer_id TEXT NULL;
ALTER TABLE branches ADD COLUMN parent_branch_name TEXT NOT NULL DEFAULT '';
`

// v6 (Phase 6): dump-based seeding for managed Postgres (Supabase, Neon, RDS)
// that allows no physical replication connections. seed_via records how a
// source is (re-)seeded: 'basebackup' (pg_basebackup, the historical default)
// or 'dump' (pg_dump piped into a fresh initdb'd cluster). dump_schemas is the
// comma-joined schema scope of a dump-seeded source (empty = whole database).
const migrateV6 = `
ALTER TABLE sources ADD COLUMN seed_via TEXT NOT NULL DEFAULT 'basebackup';
ALTER TABLE sources ADD COLUMN dump_schemas TEXT NOT NULL DEFAULT '';
`

// v7 (Phase 6): per-branch credential rotation. When the engine runs with
// --rotate-branch-credentials it ALTERs the role's password inside every
// fresh/reset branch and stores the new password here. Empty = inherit mode
// (the branch keeps the source's credentials, the historical behavior).
const migrateV7 = `
ALTER TABLE branches ADD COLUMN password TEXT NOT NULL DEFAULT '';
`

// v8 (Phase 7): a per-registry key/value meta table. Its first key,
// 'instance_id', is a stable id minted on first Open (see ensureInstanceID).
// Managed Docker/K8s resources are tagged pgbranch.instance=<id> so reconcile
// only ever reclaims resources belonging to ITS registry — concurrent
// pgbranch instances (and the IT suite's parallel packages) sharing one Docker
// daemon no longer GC each other's live containers/volumes. The next migration
// is v9.
const migrateV8 = `
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
