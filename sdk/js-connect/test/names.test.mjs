// Golden branch-name test, shared with the Go side: pgoverlayconnect (the Go
// connect helper) and internal/ghook (the GitHub App service that creates the
// branches) check the same table, so the three cannot drift apart. The Go
// test suite also runs this file when Node is on PATH.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  branchName,
  prBranchName,
  refBranchName,
  repoKey,
  sanitizeRef,
  MAX_BRANCH_NAME_LEN,
} from "../index.mjs";

const golden = JSON.parse(
  readFileSync(new URL("../../../pgoverlayconnect/testdata/branch_names.json", import.meta.url), "utf8"),
);
const validName = /^[a-z0-9][a-z0-9-]{0,40}$/;

test("golden table has every section", () => {
  assert.ok(golden.repo_keys.length > 0);
  assert.ok(golden.names.length > 0);
  assert.ok(golden.sanitize.length > 0);
});

test("repoKey matches the golden table", () => {
  for (const c of golden.repo_keys) assert.equal(repoKey(c.repo), c.key, c.repo);
});

test("branch names match the golden table", () => {
  for (const c of golden.names) {
    const got = branchName({ repo: c.repo, pr: c.pr, ref: c.ref });
    assert.equal(got, c.want, JSON.stringify(c));
    assert.match(got, validName);
    if (!c.ref) assert.equal(prBranchName(c.repo, c.pr), c.want);
  }
});

test("sanitizeRef matches the golden table", () => {
  for (const c of golden.sanitize) assert.equal(sanitizeRef(c.ref), c.want, JSON.stringify(c.ref));
});

test("derived names are always valid branch names", () => {
  const refs = ["a".repeat(40) + "/b", "a/".repeat(40), "x".repeat(200), "ab-".repeat(20), "a", "9/9", "é".repeat(50) + "z"];
  for (const ref of refs) {
    const s = sanitizeRef(ref);
    assert.ok(s.length <= MAX_BRANCH_NAME_LEN, `${ref} -> ${s}`);
    for (const repo of ["acme/widgets", "", "a-very-long-organisation/a-very-long-repository-name"]) {
      assert.match(refBranchName(repo, ref), validName, `${repo} ${ref}`);
    }
  }
});
