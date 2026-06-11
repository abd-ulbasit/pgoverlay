export interface AcquireOptions {
  /** branchd base URL. Default: PGBRANCH_SERVER. Required (here or in env). */
  server?: string;
  /** API bearer token. Default: PGBRANCH_TOKEN. */
  token?: string;
  /** Source to branch from. Default: PGBRANCH_TEST_SOURCE, else "main". */
  source?: string;
  /** Branch TTL in seconds — a server-side safety net; destroy() is the
   * primary cleanup. 0 = never reaped. Default: 3600. */
  ttlSeconds?: number;
  /** Explicit branch name (must match ^[a-z0-9][a-z0-9-]{0,40}$).
   * Default: a generated "t-js-<random>". */
  name?: string;
  /** Database password used to build dsn/proxyDsn when the server does not
   * return a per-branch one. Default: PGBRANCH_PASSWORD. */
  password?: string;
  /** Ready-poll interval in milliseconds. Default: 1000. */
  pollIntervalMs?: number;
  /** Overall ready-wait timeout in milliseconds. Default: 300000. */
  timeoutMs?: number;
}

export interface Branch {
  /** Branch name (use it with the destroy action/API). */
  branch: string;
  /** Direct Postgres host of the branch. */
  host: string;
  /** Direct Postgres port of the branch. */
  port: number;
  user: string;
  password: string;
  database: string;
  /** postgres:// URL targeting the branch directly. */
  dsn: string;
  /** postgres:// URL through the pgbranch router (server host, port 6432,
   * database "db@branch"). */
  proxyDsn: string;
  /** Delete the branch. Idempotent: a 404 (already gone) is not an error. */
  destroy(): Promise<void>;
}

/**
 * Create a copy-on-write Postgres branch on a pgbranch server and wait until
 * it is ready. Call branch.destroy() when the test finishes; the TTL reaps
 * leaked branches as a fallback.
 */
export function acquire(opts?: AcquireOptions): Promise<Branch>;
