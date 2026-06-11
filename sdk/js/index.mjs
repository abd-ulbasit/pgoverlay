// pgbranch-test: a disposable Postgres branch for your JS/TS test suite.
// Zero dependencies — Node 18+ global fetch and node:crypto only.
import { randomBytes } from "node:crypto";

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,40}$/;

function randHex(n) {
  return randomBytes(Math.ceil(n / 2))
    .toString("hex")
    .slice(0, n);
}

function dsn(user, password, host, port, database) {
  const auth = password
    ? `${encodeURIComponent(user)}:${encodeURIComponent(password)}`
    : encodeURIComponent(user);
  return `postgres://${auth}@${host}:${port}/${database}`;
}

async function request(server, token, method, path, body) {
  const res = await fetch(server + path, {
    method,
    headers: {
      authorization: `Bearer ${token}`,
      ...(body ? { "content-type": "application/json" } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  return { status: res.status, text, json: () => JSON.parse(text) };
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

/**
 * Create a copy-on-write Postgres branch on a pgbranch server and wait until
 * it is ready. See index.d.ts for the option/result shapes.
 */
export async function acquire(opts = {}) {
  const server = (opts.server ?? process.env.PGBRANCH_SERVER ?? "").replace(/\/+$/, "");
  if (!server) {
    throw new Error(
      "pgbranch-test: no server — pass {server} or set PGBRANCH_SERVER",
    );
  }
  const token = opts.token ?? process.env.PGBRANCH_TOKEN ?? "";
  const source = opts.source ?? process.env.PGBRANCH_TEST_SOURCE ?? "main";
  const ttlSeconds = opts.ttlSeconds ?? 3600;
  const name = opts.name ?? `t-js-${randHex(6)}`;
  if (!NAME_RE.test(name)) {
    throw new Error(
      `pgbranch-test: invalid branch name ${JSON.stringify(name)} (must match ${NAME_RE})`,
    );
  }
  const pollIntervalMs = opts.pollIntervalMs ?? 1000;
  const timeoutMs = opts.timeoutMs ?? 5 * 60 * 1000;

  const created = await request(server, token, "POST", "/v1/branches", {
    name,
    source,
    ttl_seconds: ttlSeconds,
  });
  if (created.status !== 201) {
    throw new Error(
      `pgbranch-test: create branch ${name}: HTTP ${created.status}: ${created.text.trim()}`,
    );
  }

  // The create endpoint returns ready synchronously today, but don't depend
  // on it: poll GET until the branch reports ready.
  let b = created.json();
  const deadline = Date.now() + timeoutMs;
  while (b.state !== "ready") {
    if (Date.now() > deadline) {
      throw new Error(
        `pgbranch-test: branch ${name} not ready after ${timeoutMs}ms (state ${JSON.stringify(b.state)})`,
      );
    }
    await sleep(pollIntervalMs);
    const got = await request(server, token, "GET", `/v1/branches/${name}`);
    if (got.status !== 200) {
      throw new Error(
        `pgbranch-test: wait for branch ${name}: HTTP ${got.status}: ${got.text.trim()}`,
      );
    }
    b = got.json();
  }

  const serverHost = new URL(server).hostname;
  const host = b.host || serverHost;
  const user = b.user || "postgres";
  const database = b.database || "postgres";
  // a server-returned per-branch password (credential rotation) wins over
  // the caller/env fallback
  const password = b.password || opts.password || process.env.PGBRANCH_PASSWORD || "";

  return {
    branch: name,
    host,
    port: b.port,
    user,
    password,
    database,
    dsn: dsn(user, password, host, b.port, database),
    proxyDsn: dsn(user, password, serverHost, 6432, b.proxy_database),
    /** Delete the branch. Safe to call twice; a 404 is not an error. */
    async destroy() {
      const res = await request(server, token, "DELETE", `/v1/branches/${name}`);
      if (res.status !== 204 && res.status !== 404) {
        throw new Error(
          `pgbranch-test: destroy branch ${name}: HTTP ${res.status}: ${res.text.trim()}`,
        );
      }
    },
  };
}
