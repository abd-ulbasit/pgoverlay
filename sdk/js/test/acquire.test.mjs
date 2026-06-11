import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { acquire } from "../index.mjs";

const NAME_RE = /^[a-z0-9][a-z0-9-]{0,40}$/;

// stub branchd: records requests, serves a configurable sequence of states.
function startStub({ states = ["ready"], createStatus = 201 } = {}) {
  const requests = [];
  let getCount = 0;
  const branch = {
    state: "ready",
    host: "10.0.0.9",
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

test("acquire reads PGBRANCH_* env when options are omitted", async () => {
  const stub = await startStub();
  const saved = { ...process.env };
  try {
    process.env.PGBRANCH_SERVER = stub.url;
    process.env.PGBRANCH_TOKEN = "env-tok";
    process.env.PGBRANCH_TEST_SOURCE = "env-src";
    process.env.PGBRANCH_PASSWORD = "env-pw";
    const b = await acquire();
    const create = stub.requests.find((r) => r.method === "POST");
    assert.equal(create.auth, "Bearer env-tok");
    assert.equal(create.body.source, "env-src");
    assert.equal(b.password, "env-pw");
  } finally {
    process.env.PGBRANCH_SERVER = saved.PGBRANCH_SERVER ?? "";
    process.env.PGBRANCH_TOKEN = saved.PGBRANCH_TOKEN ?? "";
    process.env.PGBRANCH_TEST_SOURCE = saved.PGBRANCH_TEST_SOURCE ?? "";
    process.env.PGBRANCH_PASSWORD = saved.PGBRANCH_PASSWORD ?? "";
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
  const saved = process.env.PGBRANCH_SERVER;
  delete process.env.PGBRANCH_SERVER;
  try {
    await assert.rejects(() => acquire({ token: "t" }), /server/i);
  } finally {
    if (saved !== undefined) process.env.PGBRANCH_SERVER = saved;
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
