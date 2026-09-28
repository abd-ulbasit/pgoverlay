export interface BranchNameOptions {
  /** Exact branch name. Provide this, or `repo` with `pr` and/or `ref`. */
  branch?: string;
  /**
   * GitHub repository of the pull request, "owner/name" ($GITHUB_REPOSITORY in
   * Actions; `${VERCEL_GIT_REPO_OWNER}/${VERCEL_GIT_REPO_SLUG}` on Vercel).
   * Required with `pr` or `ref`.
   */
  repo?: string;
  /** Pull request number (pr-number naming, the service default). */
  pr?: number | string;
  /**
   * The pull request's head branch, e.g. "feat/login" ($GITHUB_HEAD_REF,
   * $VERCEL_GIT_COMMIT_REF), for git-branch naming. Used over `pr` unless it
   * has no letters or digits. Not a full ref such as "refs/heads/feat/login".
   */
  ref?: string;
}

export interface ResolveOptions extends BranchNameOptions {
  /** branchd base URL, e.g. https://branchd:7070 (or $PGOVERLAY_API). */
  server?: string;
  /** API bearer token; a viewer-role token suffices (or $PGOVERLAY_TOKEN). */
  token?: string;
  /**
   * host[:port] of the pgoverlay router for proxyDsn ("[v6]:port" for IPv6);
   * defaults to the server host + :6432.
   */
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
  /** DSN through the pgoverlay wire-protocol router (database "db@branch"). */
  proxyDsn: string;
}

export function resolve(opts?: ResolveOptions): Promise<Resolved>;

/** Prefix of every branch the pgoverlay GitHub App service creates ("gh-"). */
export const GITHUB_BRANCH_PREFIX: string;
/** The engine's branch-name limit (41). */
export const MAX_BRANCH_NAME_LEN: number;

/** The branch name resolve() looks up for these options (throws when it cannot derive one). */
export function branchName(opts: BranchNameOptions): string;
/** Repository discriminator in GitHub App branch names: 6 hex chars of sha256(lowercased "owner/name"). */
export function repoKey(repo: string): string;
/** "gh-<key>-pr-<n>": the branch for pull request `pr` of `repo` in pr-number mode. */
export function prBranchName(repo: string, pr: number): string;
/** "gh-<key>-<sanitized ref>": the branch in git-branch mode, or "" when `ref` has no letters or digits. */
export function refBranchName(repo: string, ref: string): string;
/** A git ref as a branch-name fragment (at most 41 chars); not the GitHub App service's name. */
export function sanitizeRef(ref: string): string;
