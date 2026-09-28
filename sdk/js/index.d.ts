export interface AcquireOptions {
  /** branchd base URL. Default: PGOVERLAY_SERVER. Required (here or in env). */
  server?: string;
  /** API bearer token. Default: PGOVERLAY_TOKEN. */
  token?: string;
  /** Source to branch from. Default: PGOVERLAY_TEST_SOURCE, else "main". */
  source?: string;
  /** Branch TTL in whole seconds — a server-side safety net; destroy() is
   * the primary cleanup. 0 turns the safety net off: the server applies its
   * --default-ttl if one is configured and otherwise never reaps the branch.
   * The server may shorten it to its --max-ttl. Must be a non-negative
   * integer. Default: 3600. */
  ttlSeconds?: number;
  /** Explicit branch name (must match ^[a-z0-9][a-z0-9-]{0,40}$).
   * Default: a generated "t-js-<random>". */
  name?: string;
  /** Database password used to build dsn/proxyDsn when the server does not
   * return a per-branch one (branchd --rotate-branch-credentials returns
   * it; otherwise branches inherit the source's credentials).
   * Default: PGOVERLAY_PASSWORD. */
  password?: string;
  /** host[:port] of the pgoverlay router that proxyDsn connects to; the port
   * defaults to 6432 and an IPv6 literal with a port is bracketed
   * ("[fd00::1]:6432"). Default: PGOVERLAY_PROXY_HOST, else the server's
   * host — right only when the router runs next to the REST API (a single
   * branchd). The Helm chart exposes them as two Services, e.g.
   * "pgoverlay-proxy.pgoverlay-system:6432". */
  proxyHost?: string;
  /** Ready-poll interval in milliseconds. Default: 1000. */
  pollIntervalMs?: number;
  /** Overall ready-wait timeout in milliseconds. Default: 300000. */
  timeoutMs?: number;
}

export interface Branch {
  /** Branch name (use it with the destroy action/API). */
  branch: string;
  /** Direct Postgres host of the branch — reachable only from the branchd
   * host (Docker publishes branch ports on 127.0.0.1) or from inside the
   * cluster (kube pod IPs). Prefer proxyDsn. */
  host: string;
  /** Direct Postgres port of the branch (see host). */
  port: number;
  user: string;
  password: string;
  database: string;
  /** postgres:// URL targeting the branch directly (see host). */
  dsn: string;
  /** postgres:// URL through the pgoverlay router (proxyHost, else the
   * server's host, port 6432; database "db@branch"). What tests usually
   * want: one stable endpoint that works wherever the router is reachable. */
  proxyDsn: string;
  /** Delete the branch. Idempotent: a 404 (already gone) is not an error. */
  destroy(): Promise<void>;
}

/**
 * Create a copy-on-write Postgres branch on a pgoverlay server and wait until
 * it is ready. Call branch.destroy() when the test finishes; the TTL reaps
 * leaked branches as a fallback. If the branch fails, is destroyed while
 * waiting, or is not ready within timeoutMs, acquire destroys it and rejects.
 */
export function acquire(opts?: AcquireOptions): Promise<Branch>;
