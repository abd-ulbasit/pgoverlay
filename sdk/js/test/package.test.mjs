import test from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

// Guards the publishable package shape (what `npm pack` ships) without
// needing npm: every file the manifest points at exists, and the Apache-2.0
// license text travels with the package.
const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));

test("package.json ships the module, its types and the license", () => {
  assert.equal(pkg.license, "Apache-2.0");
  for (const f of ["index.mjs", "index.d.ts", "LICENSE"]) {
    assert.ok(pkg.files.includes(f), `files must list ${f}`);
    assert.ok(existsSync(join(root, f)), `${f} must exist`);
  }
  assert.match(readFileSync(join(root, "LICENSE"), "utf8"), /Apache License\s+Version 2\.0/);
});

test("package.json entry points resolve to shipped files", () => {
  const targets = [pkg.main, pkg.types, pkg.exports["."].default, pkg.exports["."].types];
  for (const t of targets) {
    const f = t.replace(/^\.\//, "");
    assert.ok(pkg.files.includes(f), `${t} is not in files`);
    assert.ok(existsSync(join(root, f)), `${t} does not exist`);
  }
});
