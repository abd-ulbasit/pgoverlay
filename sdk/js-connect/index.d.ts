export interface ResolveOptions {
  /** branchd base URL, e.g. https://branchd:7070 (or $PGBRANCH_API). */
  server?: string;
  /** API bearer token; a viewer-role token suffices (or $PGBRANCH_TOKEN). */
  token?: string;
  /** Exact branch name. Provide this or `ref`. */
  branch?: string;
  /** Git ref, sanitized to a branch name (e.g. "feat/login" -> "feat-login"). */
  ref?: string;
  /** host[:port] of the pgbranch router for proxyDsn; defaults to server host + :6432. */
  proxyHost?: string;
  /** Fallback password for inherit-mode branches (else $PGPASSWORD). */
  password?: string;
}

export interface Resolved {
  branch: string;
  host: string;
  port: number;
  user: string;
  database: string;
  /** Direct DSN to the branch's Postgres. */
  dsn: string;
  /** DSN through the pgbranch wire-protocol router (database "db@branch"). */
  proxyDsn: string;
}

export function resolve(opts?: ResolveOptions): Promise<Resolved>;
export function sanitizeRef(ref: string): string;
