// pgoverlay-connect: resolve a ready Postgres connection string for a pgoverlay
// branch by fetching its current credentials from branchd. Zero dependencies
// (Node 18+ global fetch).
//
// It reconciles per-branch credential rotation with static app config: the
// app holds the API endpoint + a scoped (viewer) token, and fetches the
// per-branch password at startup.
//
//   import { resolve } from "pgoverlay-connect";
//   const { proxyDsn } = await resolve({
//     server: process.env.PGOVERLAY_API,
//     token: process.env.PGOVERLAY_TOKEN,
//     repo: process.env.GITHUB_REPOSITORY,  // "acme/widgets"
//     ref: process.env.GITHUB_HEAD_REF,     // "feat/login" -> gh-<key>-feat-login
//     proxyHost: "proxy.example.com:6432",
//   });
//
// The branch-name functions below mirror pgoverlayconnect's names.go (Go)
// exactly; both are checked against pgoverlayconnect/testdata/branch_names.json.

import { createHash } from "node:crypto";

/** Prefix of every branch the pgoverlay GitHub App service creates. */
export const GITHUB_BRANCH_PREFIX = "gh-";
/** The engine's branch-name limit (^[a-z0-9][a-z0-9-]{0,40}$). */
export const MAX_BRANCH_NAME_LEN = 41;
const REPO_KEY_LEN = 6;
const REF_HASH_LEN = 6;
const DEFAULT_PROXY_PORT = 6432;

function shortHash(s, n) {
  return createHash("sha256").update(s, "utf8").digest("hex").slice(0, n);
}

// sanitize: ASCII letters lowercased, ASCII letters and digits kept, every
// run of other characters one dash, edges trimmed; maxLen <= 0 means no cap,
// and a cut never leaves a trailing dash. Only ASCII is case-folded because
// JavaScript and Go map Unicode case differently.
function sanitize(ref, maxLen) {
  let s = String(ref ?? "")
    .replace(/[A-Z]/g, (c) => c.toLowerCase())
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (maxLen > 0 && s.length > maxLen) s = s.slice(0, maxLen).replace(/-+$/, "");
  return s;
}

/**
 * Sanitize a git ref to a branch-name fragment of at most 41 characters. This
 * is not the name the GitHub App service creates; see refBranchName.
 */
export function sanitizeRef(ref) {
  return sanitize(ref, MAX_BRANCH_NAME_LEN);
}

/** Repository discriminator: 6 hex chars of sha256(lowercased "owner/name"). */
export function repoKey(repo) {
  return shortHash(String(repo ?? "").replace(/[A-Z]/g, (c) => c.toLowerCase()), REPO_KEY_LEN);
}

function repoPrefix(repo) {
  return `${GITHUB_BRANCH_PREFIX}${repoKey(repo)}-`;
}

/** Branch the GitHub App service creates for PR `pr` of `repo` (pr-number mode). */
export function prBranchName(repo, pr) {
  return `${repoPrefix(repo)}pr-${pr}`;
}

/**
 * Branch the GitHub App service creates in git-branch mode for a PR of `repo`
 * whose head branch is `ref`, or "" when the ref has no ASCII letter or digit
 * (the service then names the branch by PR number).
 */
export function refBranchName(repo, ref) {
  let s = sanitize(ref, 0);
  if (!s) return "";
  const prefix = repoPrefix(repo);
  const budget = MAX_BRANCH_NAME_LEN - prefix.length;
  if (s.length > budget) {
    s = `${s.slice(0, budget - REF_HASH_LEN - 1).replace(/-+$/, "")}-${shortHash(String(ref), REF_HASH_LEN)}`;
  }
  return prefix + s;
}

/**
 * The branch name resolve() looks up: `branch` verbatim, else the name the
 * GitHub App service gives a PR of `repo` — from `ref` (git-branch naming)
 * unless it has no letters or digits, else from `pr` (pr-number naming).
 */
export function branchName({ branch, repo, ref, pr } = {}) {
  if (branch) return branch;
  const prNum = pr === undefined || pr === null || pr === "" ? 0 : Number(pr);
  if (!Number.isInteger(prNum) || prNum < 0)
    throw new Error(`pgoverlay-connect: pr must be a pull request number, got "${pr}"`);
  if (!ref && !prNum) throw new Error("pgoverlay-connect: a branch, or a repo with a pr or ref, is required");
  if (!repo)
    throw new Error(
      "pgoverlay-connect: repo (owner/name, e.g. $GITHUB_REPOSITORY) is required to derive the branch name from a pr or ref",
    );
  const n = refBranchName(repo, ref ?? "");
  if (n) return n;
  if (prNum) return prBranchName(repo, prNum);
  throw new Error(`pgoverlay-connect: ref "${ref}" has no letters or digits to name a branch after; set pr`);
}

// splitProxyHost parses "host", "host:port", "[v6]:port", "[v6]" or a bare
// IPv6 address (same forms as the Go helper). A port that is present must be
// a number in 1-65535.
function splitProxyHost(hp) {
  let host = hp;
  let port;
  const bracketed = /^\[([^\]]*)\](?::(.*))?$/.exec(hp);
  if (bracketed) {
    [, host, port] = bracketed;
  } else if (hp.indexOf(":") !== -1 && hp.indexOf(":") === hp.lastIndexOf(":")) {
    [host, port] = hp.split(":");
  }
  if (port === undefined) return [host, DEFAULT_PROXY_PORT];
  const n = /^[0-9]+$/.test(port) ? Number(port) : NaN;
  if (!(n >= 1 && n <= 65535)) throw new Error(`pgoverlay-connect: proxyHost "${hp}": invalid port "${port}"`);
  return [host, n];
}

function dsn(user, password, host, port, database) {
  const auth = password
    ? `${encodeURIComponent(user)}:${encodeURIComponent(password)}`
    : encodeURIComponent(user);
  const h = host.includes(":") ? `[${host}]` : host;
  return `postgres://${auth}@${h}:${port}/${database}`;
}

/**
 * Resolve a branch's connection info. See index.d.ts for shapes. In inherit
 * mode (server returns no password) opts.password or $PGPASSWORD is required.
 */
export async function resolve(opts = {}) {
  const server = (opts.server ?? process.env.PGOVERLAY_API ?? "").replace(/\/+$/, "");
  if (!server) throw new Error("pgoverlay-connect: server (PGOVERLAY_API) is required");
  const token = opts.token ?? process.env.PGOVERLAY_TOKEN ?? "";
  if (!token) throw new Error("pgoverlay-connect: token is required");
  const name = branchName(opts);
  let [proxyHost, proxyPort] = ["", DEFAULT_PROXY_PORT];
  if (opts.proxyHost) [proxyHost, proxyPort] = splitProxyHost(opts.proxyHost);

  const res = await fetch(`${server}/v1/branches/${encodeURIComponent(name)}`, {
    headers: { authorization: `Bearer ${token}` },
  });
  const text = await res.text();
  if (res.status === 404) throw new Error(`pgoverlay-connect: branch "${name}" not found`);
  if (res.status !== 200)
    throw new Error(`pgoverlay-connect: GET branch "${name}": HTTP ${res.status}: ${text.trim()}`);
  const b = JSON.parse(text);

  // URL.hostname keeps the brackets of an IPv6 literal; dsn() adds its own.
  const serverHost = new URL(server).hostname.replace(/^\[|\]$/g, "");
  const host = b.host || serverHost;
  const user = b.user || "postgres";
  const database = b.database || "postgres";
  let password = b.password || opts.password || process.env.PGPASSWORD || "";
  if (!password)
    throw new Error(
      `pgoverlay-connect: branch "${name}" returned no password (inherit mode) and no opts.password/PGPASSWORD set`,
    );

  if (!proxyHost) proxyHost = serverHost;
  const proxyDb = b.proxy_database || `${database}@${name}`;

  return {
    branch: name,
    host,
    port: b.port,
    user,
    database,
    dsn: dsn(user, password, host, b.port, database),
    proxyDsn: dsn(user, password, proxyHost, proxyPort, proxyDb),
  };
}
