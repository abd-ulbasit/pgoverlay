"use strict";
const $ = id => document.getElementById(id);
const tokenInput = $("token");

// The token lives in sessionStorage: it survives reloads but is dropped when
// the tab closes, so a pasted (often admin) token does not linger in the
// browser profile. Earlier versions kept it in localStorage indefinitely;
// clear that copy. Storage can be unavailable (privacy modes): the page then
// just asks again.
const TOKEN_KEY = "pgoverlay.token";
try { localStorage.removeItem(TOKEN_KEY); } catch (e) {}
try { tokenInput.value = sessionStorage.getItem(TOKEN_KEY) || ""; } catch (e) {}
tokenInput.addEventListener("change", () => {
  try { sessionStorage.setItem(TOKEN_KEY, tokenInput.value); } catch (e) {}
  refresh();
});

const usageCache = new Map(); // branch name -> bytes

function msg(text, cls) {
  const el = $("msg");
  el.textContent = text || "";
  el.className = cls || "";
}

async function api(path, opts) {
  const r = await fetch(path, Object.assign({
    headers: { "Authorization": "Bearer " + tokenInput.value, "Content-Type": "application/json" },
  }, opts));
  if (r.status === 401) throw new Error("unauthorized — paste the PGOVERLAY_TOKEN above");
  if (!r.ok) {
    let m = "HTTP " + r.status;
    try { m = (await r.json()).error || m; } catch (e) {}
    throw new Error(m);
  }
  return r.status === 204 ? null : r.json();
}

const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const badge = s => `<span class="badge ${esc(s)}">${esc(s)}</span>`;

function fmtBytes(n) {
  if (n == null) return "";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  for (; n >= 1024 && i < units.length - 1; i++) n /= 1024;
  return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
}

function countdown(expiresAt) {
  if (!expiresAt) return "never";
  const ms = new Date(expiresAt) - Date.now();
  if (ms <= 0) return "expired";
  const s = Math.floor(ms / 1000);
  if (s < 3600) return Math.floor(s / 60) + "m " + (s % 60) + "s";
  if (s < 86400) return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
  return Math.floor(s / 86400) + "d " + Math.floor((s % 86400) / 3600) + "h";
}

function parseTTLSeconds(text) {
  const t = text.trim();
  if (!t) return 0;
  const m = t.match(/^(\d+)\s*([smhd]?)$/);
  if (!m) throw new Error(`invalid ttl ${JSON.stringify(t)} (use e.g. 90m, 24h, 7d)`);
  return m[1] * ({ "": 1, s: 1, m: 60, h: 3600, d: 86400 }[m[2]]);
}

async function fetchUsage(name) {
  const u = await api(`/v1/branches/${encodeURIComponent(name)}/usage`);
  usageCache.set(name, u.bytes);
  const cell = document.querySelector(`[data-usage="${name}"]`);
  if (cell) cell.textContent = fmtBytes(u.bytes);
}

function renderSources(sources) {
  const tb = $("sources");
  tb.innerHTML = sources.length ? "" : `<tr><td class="empty" colspan="5">no sources — add one with pgb source add or POST /v1/sources</td></tr>`;
  for (const s of sources) {
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td>${esc(s.name)}</td><td>${esc(s.pg_version) || "default"}</td><td>g${s.generation}</td>
      <td>${badge(s.state)}</td><td class="dim">${esc(s.user)}@${esc(s.host)}:${s.port}/${esc(s.database)}</td>
    </tr>`);
  }
  const sel = $("bsource");
  const prev = sel.value;
  sel.innerHTML = sources.map(s => `<option ${s.name === prev ? "selected" : ""}>${esc(s.name)}</option>`).join("");
}

// branchTree orders branches parent-first (depth-first) and computes each
// row's indent depth; branches whose parent is gone (or source-based) are
// roots. parent comes from the API's parent field (parent_branch_name).
function branchTree(branches) {
  const names = new Set(branches.map(b => b.name));
  const children = new Map(); // parent name -> [branch]
  const roots = [];
  for (const b of branches) {
    const p = b.parent && names.has(b.parent) ? b.parent : "";
    if (p) {
      if (!children.has(p)) children.set(p, []);
      children.get(p).push(b);
    } else {
      roots.push(b);
    }
  }
  const out = [];
  const walk = (b, depth) => {
    out.push({ b, depth });
    for (const c of children.get(b.name) || []) walk(c, depth + 1);
  };
  for (const b of roots) walk(b, 0);
  return out;
}

function renderBranches(branches) {
  const tb = $("branches");
  tb.innerHTML = branches.length ? "" : `<tr><td class="empty" colspan="7">no branches yet</td></tr>`;
  for (const { b, depth } of branchTree(branches)) {
    const indent = depth ? `<span class="dim">${"&nbsp;&nbsp;".repeat(depth)}└ </span>` : "";
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td>${indent}${esc(b.name)}</td><td class="dim">${b.parent ? "↳ " + esc(b.parent) : esc(b.source)}</td><td>${badge(b.state)}</td>
      <td>${b.host ? esc(b.host) + ":" + b.port : '<span class="dim">—</span>'}
          <span class="dim">db ${esc(b.proxy_database)}</span></td>
      <td data-expires="${esc(b.expires_at)}">${countdown(b.expires_at)}</td>
      <td data-usage="${esc(b.name)}">${fmtBytes(usageCache.get(b.name)) || '<span class="dim">…</span>'}</td>
      <td><div class="rowbtns">
        <button data-act="usage" data-name="${esc(b.name)}" title="re-measure rw layer (runs a helper container)">du</button>
        <button data-act="reset" data-name="${esc(b.name)}" title="discard writes, re-clone from source">reset</button>
        <button class="danger" data-act="destroy" data-name="${esc(b.name)}">destroy</button>
      </div></td>
    </tr>`);
    if (b.state === "ready" && !usageCache.has(b.name)) fetchUsage(b.name).catch(() => {});
  }
  // parent dropdown: branch off a ready branch's current state instead of the source
  const sel = $("bparent");
  const prev = sel.value;
  sel.innerHTML = `<option value="">(from source)</option>` +
    branches.filter(b => b.state === "ready")
      .map(b => `<option value="${esc(b.name)}" ${b.name === prev ? "selected" : ""}>from branch ${esc(b.name)}</option>`)
      .join("");
}

async function refresh() {
  try {
    const [sources, branches] = await Promise.all([api("/v1/sources"), api("/v1/branches")]);
    renderSources(sources);
    renderBranches(branches);
    msg("");
  } catch (e) {
    msg(e.message, "err");
  }
}

$("branches").addEventListener("click", async ev => {
  const btn = ev.target.closest("button[data-act]");
  if (!btn) return;
  const name = btn.dataset.name;
  try {
    if (btn.dataset.act === "usage") {
      btn.disabled = true;
      await fetchUsage(name).finally(() => { btn.disabled = false; });
    } else if (btn.dataset.act === "reset") {
      // reset throws away every write in the branch: confirm like destroy
      if (!confirm(`reset branch ${name}? every write made in it will be discarded`)) return;
      msg(`resetting ${name}…`, "ok");
      await api(`/v1/branches/${encodeURIComponent(name)}/reset`, { method: "POST" });
      usageCache.delete(name);
      msg(`branch ${name} reset`, "ok");
    } else if (btn.dataset.act === "destroy") {
      if (!confirm(`destroy branch ${name}?`)) return;
      await api(`/v1/branches/${encodeURIComponent(name)}`, { method: "DELETE" });
      usageCache.delete(name);
      msg(`branch ${name} destroyed`, "ok");
    }
    refresh();
  } catch (e) {
    msg(e.message, "err");
  }
});

$("create").addEventListener("submit", async ev => {
  ev.preventDefault();
  try {
    const body = {
      name: $("bname").value.trim(),
      ttl_seconds: parseTTLSeconds($("bttl").value),
    };
    // a chosen parent branch wins over the source (the API takes exactly one)
    const parent = $("bparent").value;
    if (parent) body.parent = parent; else body.source = $("bsource").value;
    msg(`creating ${body.name}… (clone is instant, Postgres startup takes a few seconds${parent ? `; branching from ${parent} briefly restarts it` : ""})`, "ok");
    await api("/v1/branches", { method: "POST", body: JSON.stringify(body) });
    $("bname").value = "";
    msg(`branch ${body.name} ready`, "ok");
    refresh();
  } catch (e) {
    msg(e.message, "err");
  }
});

// countdowns tick every second; full data refresh every 5s
setInterval(() => {
  for (const td of document.querySelectorAll("[data-expires]")) {
    td.textContent = countdown(td.dataset.expires);
  }
}, 1000);
setInterval(refresh, 5000);
refresh();
