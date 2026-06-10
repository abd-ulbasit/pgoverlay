package registry

// Schema is versioned via PRAGMA user_version: migrations[i] upgrades a
// database at version i to version i+1. Phase 1 shipped with user_version 0
// and the v1 tables already created, so schemaV1 stays IF NOT EXISTS — it is
// a no-op on an existing P1 database and a full create on a fresh one.
var migrations = []string{schemaV1, migrateV2, migrateV3, migrateV4}

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
