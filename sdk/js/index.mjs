// pgoverlay-test: a disposable Postgres branch for your JS/TS test suite.
// Zero dependencies — Node 18+ global fetch and node:crypto only.
import { randomBytes } from "node:crypto";

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,40}$/;
const DEFAULT_PROXY_PORT = 6432;

// States a branch can never leave for "ready": waiting on them is pointless.
const TERMINAL_STATES = new Set(["failed", "destroying", "destroyed"]);

function randHex(n) {
  return randomBytes(Math.ceil(n / 2))
    .toString("hex")
    .slice(0, n);
}

// unbracket strips the [] around an IPv6 literal (WHATWG URL hostnames keep
// them: new URL("http://[::1]:7070").hostname === "[::1]").
function unbracket(host) {
  return host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
}

// hostPort joins host and port for a URL authority, bracketing IPv6 literals.
function hostPort(host, port) {
  const h = unbracket(host);
  return h.includes(":") ? `[${h}]:${port}` : `${h}:${port}`;
}

// parseProxyHost parses host[:port]; the port defaults to 6432. IPv6
// literals are accepted bare ("fd00::1"), bracketed ("[fd00::1]") or with a
// port ("[fd00::1]:6432").
function parseProxyHost(value) {
  const bad = () =>
    new Error(`pgoverlay-test: invalid proxyHost ${JSON.stringify(value)} (want host[:port])`);
  let host = value;
  let port = DEFAULT_PROXY_PORT;
  const m = /^\[([^\]]*)\](?::(.*))?$/.exec(value);
  if (m) {
    host = m[1];
    if (m[2] !== undefined) port = m[2];
  } else if ((value.match(/:/g) || []).length === 1) {
    [host, port] = value.split(":");
  }
  if (typeof port === "string") {
    if (!/^[0-9]+$/.test(port)) throw bad();
    port = Number(port);
  }
  if (!host || /[\s/@[\]]/.test(host) || port < 1 || port > 65535) throw bad();
  return { host, port };
}

// dsn builds postgres://user[:password]@host:port/db. encodeURIComponent
// yields the %XX userinfo URL parsers decode (a space is %20, never '+'); the
// database may contain '@' (proxy routing), which is legal in a URL path.
function dsn(user, password, host, port, database) {
  const auth = password
    ? `${encodeURIComponent(user)}:${encodeURIComponent(password)}`
    : encodeURIComponent(user);
  return `postgres://${auth}@${hostPort(host, port)}/${database}`;
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

// lastReason returns ": <reason>" for the branch's most recent recorded
// transition, or "" — best-effort, it only enriches an error message.
async function lastReason(server, token, name) {
  try {
    const res = await request(server, token, "GET", `/v1/branches/${encodeURIComponent(name)}/history`);
    if (res.status !== 200) return "";
    const hist = res.json();
    for (let i = hist.length - 1; i >= 0; i--) {
      if (hist[i] && hist[i].reason) return `: ${hist[i].reason}`;
    }
  } catch {
    // no history: the state alone has to do
  }
  return "";
}

/**
 * Create a copy-on-write Postgres branch on a pgoverlay server and wait until
 * it is ready. See index.d.ts for the option/result shapes.
 */
export async function acquire(opts = {}) {
  const server = (opts.server ?? process.env.PGOVERLAY_SERVER ?? "").replace(/\/+$/, "");
  if (!server) {
    throw new Error(
      "pgoverlay-test: no server — pass {server} or set PGOVERLAY_SERVER",
    );
  }
  const token = opts.token ?? process.env.PGOVERLAY_TOKEN ?? "";
  const source = opts.source ?? process.env.PGOVERLAY_TEST_SOURCE ?? "main";
  const ttlSeconds = opts.ttlSeconds ?? 3600;
  if (!Number.isInteger(ttlSeconds) || ttlSeconds < 0) {
    throw new Error(
      `pgoverlay-test: ttlSeconds must be a non-negative integer, got ${JSON.stringify(ttlSeconds)}`,
    );
  }
  const name = opts.name ?? `t-js-${randHex(6)}`;
  if (!NAME_RE.test(name)) {
    throw new Error(
      `pgoverlay-test: invalid branch name ${JSON.stringify(name)} (must match ${NAME_RE})`,
    );
  }
  const serverHost = unbracket(new URL(server).hostname);
  const proxyHostOpt = opts.proxyHost ?? process.env.PGOVERLAY_PROXY_HOST ?? "";
  const proxy = proxyHostOpt
    ? parseProxyHost(proxyHostOpt)
    : { host: serverHost, port: DEFAULT_PROXY_PORT };
  const pollIntervalMs = opts.pollIntervalMs ?? 1000;
  const timeoutMs = opts.timeoutMs ?? 5 * 60 * 1000;
  const path = `/v1/branches/${encodeURIComponent(name)}`;

  const created = await request(server, token, "POST", "/v1/branches", {
    name,
    source,
    ttl_seconds: ttlSeconds,
  });
  if (created.status !== 201) {
    throw new Error(
      `pgoverlay-test: create branch ${name}: HTTP ${created.status}: ${created.text.trim()}`,
    );
  }

  async function destroy() {
    const res = await request(server, token, "DELETE", path);
    if (res.status !== 204 && res.status !== 404) {
      throw new Error(
        `pgoverlay-test: destroy branch ${name}: HTTP ${res.status}: ${res.text.trim()}`,
      );
    }
  }

  // The create endpoint returns ready synchronously today, but don't depend
  // on it: poll GET until the branch reports ready, and stop at once on a
  // state it can never leave for ready. The caller never gets a handle to a
  // branch that failed to become ready, so destroy it here (best-effort; the
  // TTL is the fallback).
  let b;
  try {
    b = created.json();
    const deadline = Date.now() + timeoutMs;
    while (b.state !== "ready") {
      if (TERMINAL_STATES.has(b.state)) {
        throw new Error(
          `pgoverlay-test: branch ${name} is ${b.state} and will never become ready${await lastReason(server, token, name)}`,
        );
      }
      if (Date.now() > deadline) {
        throw new Error(
          `pgoverlay-test: branch ${name} not ready after ${timeoutMs}ms (state ${JSON.stringify(b.state)})`,
        );
      }
      await sleep(pollIntervalMs);
      const got = await request(server, token, "GET", path);
      if (got.status !== 200) {
        throw new Error(
          `pgoverlay-test: wait for branch ${name}: HTTP ${got.status}: ${got.text.trim()}`,
        );
      }
      b = got.json();
    }
  } catch (err) {
    await destroy().catch(() => {});
    throw err;
  }

  const host = b.host || serverHost;
  const user = b.user || "postgres";
  const database = b.database || "postgres";
  const proxyDatabase = b.proxy_database || `${database}@${name}`;
  // a server-returned per-branch password (credential rotation) wins over
  // the caller/env fallback
  const password = b.password || opts.password || process.env.PGOVERLAY_PASSWORD || "";

  return {
    branch: name,
    host,
    port: b.port,
    user,
    password,
    database,
    dsn: dsn(user, password, host, b.port, database),
    proxyDsn: dsn(user, password, proxy.host, proxy.port, proxyDatabase),
    /** Delete the branch. Safe to call twice; a 404 is not an error. */
    destroy,
  };
}
