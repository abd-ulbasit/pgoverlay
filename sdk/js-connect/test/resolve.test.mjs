import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { resolve, sanitizeRef } from "../index.mjs";

// stubServer answers GET /v1/branches/{name} with the given branch JSON
// (status 404 when `branch` is null), recording the last request.
function stubServer(branch) {
  const seen = { auth: null, name: null };
  const srv = createServer((req, res) => {
    seen.auth = req.headers["authorization"];
    seen.name = decodeURIComponent(req.url.replace("/v1/branches/", ""));
    if (!branch) {
      res.statusCode = 404;
      return res.end("not found");
    }
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(branch));
  });
  return new Promise((r) => srv.listen(0, "127.0.0.1", () => r({ srv, seen, port: srv.address().port })));
}

test("resolve builds DSNs from a rotated password and sanitizes the ref", async () => {
  const { srv, seen, port } = await stubServer({
    name: "feat-login", host: "10.0.0.5", port: 5432, user: "app",
    password: "rot123", database: "appdb", proxy_database: "appdb@feat-login",
  });
  try {
    const r = await resolve({
      server: `http://127.0.0.1:${port}`, token: "tok",
      ref: "feat/Login", proxyHost: "proxy.example.com:6432",
    });
    assert.equal(seen.auth, "Bearer tok");
    assert.equal(seen.name, "feat-login");
    assert.equal(r.dsn, "postgres://app:rot123@10.0.0.5:5432/appdb");
    assert.equal(r.proxyDsn, "postgres://app:rot123@proxy.example.com:6432/appdb@feat-login");
  } finally {
    srv.close();
  }
});

test("inherit mode requires a password", async () => {
  const { srv, port } = await stubServer({
    name: "main-stable", host: "h", port: 5432, user: "postgres",
    database: "postgres", proxy_database: "postgres@main-stable",
  });
  try {
    await assert.rejects(
      resolve({ server: `http://127.0.0.1:${port}`, token: "t", branch: "main-stable" }),
      /no password/,
    );
    const r = await resolve({
      server: `http://127.0.0.1:${port}`, token: "t", branch: "main-stable", password: "given",
    });
    assert.match(r.dsn, /:given@/);
  } finally {
    srv.close();
  }
});

test("404 branch is a clear error", async () => {
  const { srv, port } = await stubServer(null);
  try {
    await assert.rejects(
      resolve({ server: `http://127.0.0.1:${port}`, token: "t", branch: "nope" }),
      /not found/,
    );
  } finally {
    srv.close();
  }
});

test("validation", async () => {
  await assert.rejects(resolve({ token: "t", branch: "b" }), /server/);
  await assert.rejects(resolve({ server: "http://x", branch: "b" }), /token/);
  await assert.rejects(resolve({ server: "http://x", token: "t" }), /branch or ref/);
});

test("sanitizeRef", () => {
  assert.equal(sanitizeRef("feat/Login"), "feat-login");
  assert.equal(sanitizeRef("FIX--x//y!"), "fix-x-y");
  assert.equal(sanitizeRef("-/-"), "");
  assert.equal(sanitizeRef("a".repeat(60)), "a".repeat(41));
});
