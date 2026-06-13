// pgbranch-connect: resolve a ready Postgres connection string for a pgbranch
// branch by fetching its current credentials from branchd. Zero dependencies
// (Node 18+ global fetch).
//
// It reconciles per-branch credential rotation with static app config: the
// app holds the API endpoint + a scoped (viewer) token, and fetches the
// per-branch password at startup.
//
//   import { resolve } from "pgbranch-connect";
//   const { proxyDsn } = await resolve({
//     server: process.env.PGBRANCH_API,
//     token: process.env.PGBRANCH_TOKEN,
//     ref: process.env.GIT_REF,            // "feat/login" -> feat-login
//     proxyHost: "proxy.example.com:6432",
//   });

/** Sanitize a git ref to a pgbranch branch name (^[a-z0-9][a-z0-9-]{0,40}$). */
export function sanitizeRef(ref) {
  return (ref ?? "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 41)
    .replace(/-+$/g, "");
}

function dsn(user, password, host, port, database) {
  const auth = password
    ? `${encodeURIComponent(user)}:${encodeURIComponent(password)}`
    : encodeURIComponent(user);
  return `postgres://${auth}@${host}:${port}/${database}`;
}

/**
 * Resolve a branch's connection info. See index.d.ts for shapes. In inherit
 * mode (server returns no password) opts.password or $PGPASSWORD is required.
 */
export async function resolve(opts = {}) {
  const server = (opts.server ?? process.env.PGBRANCH_API ?? "").replace(/\/+$/, "");
  if (!server) throw new Error("pgbranch-connect: server (PGBRANCH_API) is required");
  const token = opts.token ?? process.env.PGBRANCH_TOKEN ?? "";
  if (!token) throw new Error("pgbranch-connect: token is required");
  const name = opts.branch || sanitizeRef(opts.ref);
  if (!name) throw new Error("pgbranch-connect: a branch or ref is required");

  const res = await fetch(`${server}/v1/branches/${encodeURIComponent(name)}`, {
    headers: { authorization: `Bearer ${token}` },
  });
  const text = await res.text();
  if (res.status === 404) throw new Error(`pgbranch-connect: branch "${name}" not found`);
  if (res.status !== 200)
    throw new Error(`pgbranch-connect: GET branch "${name}": HTTP ${res.status}: ${text.trim()}`);
  const b = JSON.parse(text);

  const serverHost = new URL(server).hostname;
  const host = b.host || serverHost;
  const user = b.user || "postgres";
  const database = b.database || "postgres";
  let password = b.password || opts.password || process.env.PGPASSWORD || "";
  if (!password)
    throw new Error(
      `pgbranch-connect: branch "${name}" returned no password (inherit mode) and no opts.password/PGPASSWORD set`,
    );

  let proxyHost = serverHost,
    proxyPort = 6432;
  if (opts.proxyHost) {
    const [h, p] = opts.proxyHost.split(":");
    proxyHost = h;
    if (p) proxyPort = Number(p);
  }
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
