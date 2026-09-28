import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { resolve, branchName } from "../index.mjs";

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

test("resolve builds DSNs from a rotated password and derives the GitHub App name", async () => {
  const { srv, seen, port } = await stubServer({
    name: "gh-d782c8-feat-login", host: "10.0.0.5", port: 5432, user: "app",
    password: "rot123", database: "appdb", proxy_database: "appdb@gh-d782c8-feat-login",
  });
  try {
    const r = await resolve({
      server: `http://127.0.0.1:${port}`, token: "tok",
      repo: "acme/widgets", ref: "feat/Login", proxyHost: "proxy.example.com:6432",
    });
    assert.equal(seen.auth, "Bearer tok");
    assert.equal(seen.name, "gh-d782c8-feat-login");
    assert.equal(r.dsn, "postgres://app:rot123@10.0.0.5:5432/appdb");
    assert.equal(r.proxyDsn, "postgres://app:rot123@proxy.example.com:6432/appdb@gh-d782c8-feat-login");
  } finally {
    srv.close();
  }
});

test("resolve looks up the pr-number name, and falls back to it for an empty ref", async () => {
  const { srv, seen, port } = await stubServer({ name: "x", host: "h", port: 5432, password: "p" });
  try {
    const server = `http://127.0.0.1:${port}`;
    for (const [opts, want] of [
      [{ repo: "acme/widgets", pr: 7 }, "gh-d782c8-pr-7"],
      [{ repo: "Acme/Widgets", pr: "7" }, "gh-d782c8-pr-7"],
      [{ repo: "acme/gadgets", pr: 7 }, "gh-9c2435-pr-7"],
      [{ repo: "acme/widgets", ref: "-/-", pr: 9 }, "gh-d782c8-pr-9"],
      [{ branch: "exact-name", repo: "acme/widgets", pr: 7 }, "exact-name"],
    ]) {
      await resolve({ server, token: "t", ...opts });
      assert.equal(seen.name, want, JSON.stringify(opts));
    }
  } finally {
    srv.close();
  }
});

test("proxyHost forms and IPv6 hosts", async () => {
  const { srv, port } = await stubServer({
    name: "b", host: "fd00::5", port: 31234, user: "u", password: "p",
    database: "db", proxy_database: "db@b",
  });
  try {
    const server = `http://127.0.0.1:${port}`;
    for (const [proxy, want] of [
      [undefined, "postgres://u:p@127.0.0.1:6432/db@b"],
      ["proxy", "postgres://u:p@proxy:6432/db@b"],
      ["proxy:7000", "postgres://u:p@proxy:7000/db@b"],
      ["[::1]:6433", "postgres://u:p@[::1]:6433/db@b"],
      ["[::1]", "postgres://u:p@[::1]:6432/db@b"],
      ["fd00::1", "postgres://u:p@[fd00::1]:6432/db@b"],
    ]) {
      const r = await resolve({ server, token: "t", branch: "b", proxyHost: proxy });
      assert.equal(r.proxyDsn, want, String(proxy));
      assert.equal(r.dsn, "postgres://u:p@[fd00::5]:31234/db");
    }
    for (const bad of ["proxy:abc", "proxy:", "proxy:0", "proxy:70000", "[::1]:x"]) {
      await assert.rejects(resolve({ server, token: "t", branch: "b", proxyHost: bad }), /invalid port/, bad);
    }
  } finally {
    srv.close();
  }
});

test("inherit mode requires a password", async () => {
  const { srv, port } = await stubServer({
    name: "main-stable", host: "h", port: 5432, user: "postgres",
    database: "postgres", proxy_database: "postgres@main-stable",
  });
  const saved = process.env.PGPASSWORD;
  delete process.env.PGPASSWORD;
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
    if (saved !== undefined) process.env.PGPASSWORD = saved;
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
  await assert.rejects(resolve({ server: "http://x", token: "t" }), /branch, or a repo/);
  await assert.rejects(resolve({ server: "http://x", token: "t", pr: 7 }), /repo/);
  await assert.rejects(resolve({ server: "http://x", token: "t", ref: "feat/x" }), /repo/);
  await assert.rejects(resolve({ server: "http://x", token: "t", repo: "a/b", ref: "-/-" }), /set pr/);
  assert.throws(() => branchName({ repo: "a/b", pr: "seven" }), /pull request number/);
});
