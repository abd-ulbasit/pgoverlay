import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { acquire } from "../index.mjs";

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,40}$/;

// stub branchd: records requests, serves a configurable sequence of states.
function startStub({
  states = ["ready"],
  createStatus = 201,
  host = "10.0.0.9",
  reason = "",
} = {}) {
  const requests = [];
  let getCount = 0;
  const branch = {
    state: "ready",
    host,
    port: 31555,
    user: "appuser",
    database: "appdb",
  };
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const rec = {
        method: req.method,
        url: req.url,
        auth: req.headers.authorization,
        body: body ? JSON.parse(body) : null,
      };
      requests.push(rec);
      res.setHeader("content-type", "application/json");
      if (req.method === "POST" && req.url === "/v1/branches") {
        if (createStatus !== 201) {
          res.statusCode = createStatus;
          res.end(JSON.stringify({ error: "boom" }));
          return;
        }
        res.statusCode = 201;
        res.end(
          JSON.stringify({
            ...branch,
            name: rec.body.name,
            state: states[0],
            proxy_database: `${branch.database}@${rec.body.name}`,
          }),
        );
      } else if (req.method === "GET" && req.url.endsWith("/history")) {
        res.end(
          JSON.stringify([
            { from_state: "", to_state: "creating", reason: "create" },
            { from_state: "creating", to_state: states[0], reason },
          ]),
        );
      } else if (req.method === "GET" && req.url.startsWith("/v1/branches/")) {
        if (states.length > 1) states.shift();
        const state = states[0];
        getCount++;
        const name = req.url.split("/").pop();
        res.end(
          JSON.stringify({
            ...branch,
            name,
            state,
            proxy_database: `${branch.database}@${name}`,
          }),
        );
      } else if (req.method === "DELETE" && req.url.startsWith("/v1/branches/")) {
        res.statusCode = 204;
        res.end();
      } else {
        res.statusCode = 404;
        res.end(JSON.stringify({ error: "not found" }));
      }
    });
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      resolve({
        url: `http://127.0.0.1:${server.address().port}`,
        requests,
        getCount: () => getCount,
        close: () => new Promise((r) => server.close(r)),
      });
    });
  });
}

test("acquire creates a branch with defaults and returns connection details", async () => {
  const stub = await startStub();
  try {
    const b = await acquire({ server: stub.url, token: "tok-js", password: "pw" });
    const create = stub.requests.find((r) => r.method === "POST");
    assert.equal(create.auth, "Bearer tok-js");
    assert.equal(create.body.source, "main");
    assert.equal(create.body.ttl_seconds, 3600);
    assert.equal(create.body.name, b.branch);
    assert.match(b.branch, NAME_RE);
    assert.ok(b.branch.startsWith("t-"), `name ${b.branch} must be t- prefixed`);
    assert.ok(b.branch.length <= 41);

    assert.equal(b.host, "10.0.0.9");
    assert.equal(b.port, 31555);
    assert.equal(b.user, "appuser");
    assert.equal(b.database, "appdb");
    assert.equal(b.password, "pw");
    assert.equal(b.dsn, `postgres://appuser:pw@10.0.0.9:31555/appdb`);
    assert.equal(b.proxyDsn, `postgres://appuser:pw@127.0.0.1:6432/appdb@${b.branch}`);
  } finally {
    await stub.close();
  }
});

test("acquire honors explicit options", async () => {
  const stub = await startStub();
  try {
    const b = await acquire({
      server: stub.url + "/", // trailing slash tolerated
      token: "tok-js",
      source: "staging",
      ttlSeconds: 120,
      name: "my-explicit-name",
    });
    const create = stub.requests.find((r) => r.method === "POST");
    assert.equal(create.body.source, "staging");
    assert.equal(create.body.ttl_seconds, 120);
    assert.equal(create.body.name, "my-explicit-name");
    assert.equal(b.branch, "my-explicit-name");
  } finally {
    await stub.close();
  }
});

test("acquire reads PGOVERLAY_* env when options are omitted", async () => {
  const stub = await startStub();
  const saved = { ...process.env };
  try {
    process.env.PGOVERLAY_SERVER = stub.url;
    process.env.PGOVERLAY_TOKEN = "env-tok";
    process.env.PGOVERLAY_TEST_SOURCE = "env-src";
    process.env.PGOVERLAY_PASSWORD = "env-pw";
    const b = await acquire();
    const create = stub.requests.find((r) => r.method === "POST");
    assert.equal(create.auth, "Bearer env-tok");
    assert.equal(create.body.source, "env-src");
    assert.equal(b.password, "env-pw");
  } finally {
    process.env.PGOVERLAY_SERVER = saved.PGOVERLAY_SERVER ?? "";
    process.env.PGOVERLAY_TOKEN = saved.PGOVERLAY_TOKEN ?? "";
    process.env.PGOVERLAY_TEST_SOURCE = saved.PGOVERLAY_TEST_SOURCE ?? "";
    process.env.PGOVERLAY_PASSWORD = saved.PGOVERLAY_PASSWORD ?? "";
    await stub.close();
  }
});

test("acquire polls GET until the branch is ready", async () => {
  const stub = await startStub({ states: ["creating", "creating", "ready"] });
  try {
    const b = await acquire({
      server: stub.url,
      token: "t",
      pollIntervalMs: 1,
    });
    assert.equal(b.host, "10.0.0.9");
    assert.ok(stub.getCount() >= 1, "expected at least one GET poll");
  } finally {
    await stub.close();
  }
});

test("acquire rejects when the server is missing", async () => {
  const saved = process.env.PGOVERLAY_SERVER;
  delete process.env.PGOVERLAY_SERVER;
  try {
    await assert.rejects(() => acquire({ token: "t" }), /server/i);
  } finally {
    if (saved !== undefined) process.env.PGOVERLAY_SERVER = saved;
  }
});

test("acquire surfaces server errors with the response body", async () => {
  const stub = await startStub({ createStatus: 409 });
  try {
    await assert.rejects(
      () => acquire({ server: stub.url, token: "t" }),
      /409.*boom/s,
    );
  } finally {
    await stub.close();
  }
});

test("destroy() deletes the branch and tolerates 404", async () => {
  const stub = await startStub();
  try {
    const b = await acquire({ server: stub.url, token: "tok-js" });
    await b.destroy();
    const del = stub.requests.find((r) => r.method === "DELETE");
    assert.ok(del, "expected a DELETE request");
    assert.equal(del.url, `/v1/branches/${b.branch}`);
    assert.equal(del.auth, "Bearer tok-js");
    await b.destroy(); // second call: stub still answers, must not throw
  } finally {
    await stub.close();
  }
});

test("dsn and proxyDsn percent-encode credentials so they parse back exactly", async () => {
  const stub = await startStub();
  const pw = "pass word@:/?#+%";
  try {
    const b = await acquire({ server: stub.url, token: "t", password: pw });
    for (const d of [b.dsn, b.proxyDsn]) {
      assert.ok(!d.includes("+"), `${d}: a '+' would not decode to a space`);
      const u = new URL(d);
      assert.equal(decodeURIComponent(u.password), pw, d);
      assert.equal(decodeURIComponent(u.username), "appuser", d);
    }
  } finally {
    await stub.close();
  }
});

test("an IPv6 branch host is bracketed in the dsn", async () => {
  const stub = await startStub({ host: "fd00::7" });
  try {
    const b = await acquire({ server: stub.url, token: "t", password: "pw" });
    assert.equal(b.host, "fd00::7");
    assert.equal(b.dsn, "postgres://appuser:pw@[fd00::7]:31555/appdb");
    assert.equal(new URL(b.dsn).port, "31555");
  } finally {
    await stub.close();
  }
});

test("proxyDsn targets proxyHost / PGOVERLAY_PROXY_HOST when set", async () => {
  const stub = await startStub();
  const saved = process.env.PGOVERLAY_PROXY_HOST;
  try {
    process.env.PGOVERLAY_PROXY_HOST = "pgoverlay-proxy.pgoverlay-system:7432";
    let b = await acquire({ server: stub.url, token: "t", password: "pw" });
    assert.equal(
      b.proxyDsn,
      `postgres://appuser:pw@pgoverlay-proxy.pgoverlay-system:7432/appdb@${b.branch}`,
    );

    // the option wins over the env; the port defaults to 6432
    b = await acquire({ server: stub.url, token: "t", password: "pw", proxyHost: "proxy.internal" });
    assert.equal(b.proxyDsn, `postgres://appuser:pw@proxy.internal:6432/appdb@${b.branch}`);

    for (const [proxyHost, authority] of [
      ["[fd00::1]:6433", "[fd00::1]:6433"],
      ["[fd00::1]", "[fd00::1]:6432"],
      ["fd00::1", "[fd00::1]:6432"],
    ]) {
      b = await acquire({ server: stub.url, token: "t", password: "pw", proxyHost });
      assert.equal(b.proxyDsn, `postgres://appuser:pw@${authority}/appdb@${b.branch}`, proxyHost);
    }

    // a malformed value rejects before any branch is created
    const posts = stub.requests.filter((r) => r.method === "POST").length;
    for (const proxyHost of ["proxy:notaport", "proxy:", ":6432", "proxy:70000", "http://proxy"]) {
      await assert.rejects(
        () => acquire({ server: stub.url, token: "t", proxyHost }),
        /invalid proxyHost/,
        proxyHost,
      );
    }
    assert.equal(stub.requests.filter((r) => r.method === "POST").length, posts);
  } finally {
    if (saved === undefined) delete process.env.PGOVERLAY_PROXY_HOST;
    else process.env.PGOVERLAY_PROXY_HOST = saved;
    await stub.close();
  }
});

for (const state of ["failed", "destroying", "destroyed"]) {
  test(`acquire fails fast on a ${state} branch, with the reason, and destroys it`, async () => {
    const stub = await startStub({
      states: ["creating", state],
      reason: "clone failed: no space left on device",
    });
    try {
      await assert.rejects(
        () => acquire({ server: stub.url, token: "t", pollIntervalMs: 1, timeoutMs: 60_000 }),
        new RegExp(`is ${state} .*no space left on device`),
      );
      assert.equal(stub.getCount(), 1, "must stop polling at the first terminal state");
      assert.ok(
        stub.requests.some((r) => r.method === "DELETE"),
        "the branch must be destroyed: the caller never got a handle to it",
      );
    } finally {
      await stub.close();
    }
  });
}

test("acquire destroys a branch that never becomes ready", async () => {
  const stub = await startStub({ states: ["creating"] });
  try {
    await assert.rejects(
      () => acquire({ server: stub.url, token: "t", pollIntervalMs: 1, timeoutMs: 20 }),
      /not ready after 20ms/,
    );
    assert.ok(stub.requests.some((r) => r.method === "DELETE"), "expected a best-effort DELETE");
  } finally {
    await stub.close();
  }
});

test("ttlSeconds must be a non-negative integer; 0 is sent as-is", async () => {
  const stub = await startStub();
  try {
    for (const ttlSeconds of [-1, 0.5, "60"]) {
      await assert.rejects(
        () => acquire({ server: stub.url, token: "t", ttlSeconds }),
        /ttlSeconds must be a non-negative integer/,
        String(ttlSeconds),
      );
    }
    assert.equal(stub.requests.length, 0, "no request for an invalid ttlSeconds");
    await acquire({ server: stub.url, token: "t", ttlSeconds: 0 });
    assert.equal(stub.requests.find((r) => r.method === "POST").body.ttl_seconds, 0);
  } finally {
    await stub.close();
  }
});
