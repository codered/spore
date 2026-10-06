"use strict";

// spore's whole front end. No framework, no build step: the daemon serves
// this file straight out of the binary.
//
// Every API call passes api() a method and a literal path template such as
// /api/sessions/{id}/stop. web_test.go reads this file for those literals and
// checks each against the daemon's routes, so keep them literal.

// ---------- state ----------

const S = {
  // id -> session record: the listed SessionJSON plus live state from the
  // global feed (working, approvals).
  sessions: new Map(),
  open: null,          // the session on screen
  view: "chat",        // chat | skills | agents | jobs | usage | refinements
  filter: "",          // sidebar session filter
  order: [],           // visible session ids, sidebar order, for j/k/b
  jobs: [],
  keys: { enabled: true, hints: true },
  // approvals this page answered, so their `resolved` echo is not reported
  // as answered elsewhere.
  answered: new Set(),
  generation: 0,
  // the open transcript's live pieces
  live: null,          // the prose node text deltas append to
  tools: new Map(),    // tool_use id -> row
  cursor: null,        // the tool row o toggles
  head: { model: "", ctx: 0, cost: 0 },
  vstate: {},          // per-view table state: sort, filter, selected
  vtimer: null,
};

const VIEWS = [
  ["chat", "Chat", "C"],
  ["skills", "Skills", "S"],
  ["agents", "Agents", "A"],
  ["jobs", "Jobs", "J"],
  ["usage", "Usage", "U"],
  ["refinements", "Refinements", "R"],
];

const el = (id) => document.getElementById(id);

function h(tag, attrs, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid === undefined || kid === null || kid === false) continue;
    n.appendChild(typeof kid === "string" ? document.createTextNode(kid) : kid);
  }
  return n;
}

function hint(key) {
  return h("kbd", { class: "hint", text: key });
}

function setStatus(text, isError) {
  const node = el("status");
  node.textContent = text || "";
  node.className = isError ? "error" : "";
}

async function api(method, tpl, params, body) {
  const path = tpl.replace(/\{(\w+)\}/g, (_, k) => encodeURIComponent(params[k]));
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); } catch (e) { payload = null; }
  }
  if (!res.ok) {
    if (res.status === 401) {
      // The daemon wants its token: this tab was opened without spore web,
      // or the token was rotated.
      const err = new Error("Signed out: run `spore web` in a terminal to open the UI again.");
      err.status = 401;
      throw err;
    }
    const err = new Error(payload && payload.error ? payload.error : res.status + " " + res.statusText);
    err.status = res.status;
    throw err;
  }
  return payload;
}

// ---------- formatting ----------

function cut(s, n) {
  s = s || "";
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

function tailPath(s, n) {
  s = s || "";
  return s.length > n ? "…" + s.slice(s.length - n + 1) : s;
}

function kTokens(n) {
  n = n || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(n >= 1e4 ? 0 : 1) + "k";
  return String(n);
}

function money(x) {
  return "$" + (x || 0).toFixed(4);
}

function ago(t) {
  if (!t) return "-";
  const d = new Date(t);
  if (isNaN(d) || d.getFullYear() < 2000) return "-";
  return span((Date.now() - d.getTime()) / 1000);
}

function until(t) {
  if (!t) return "-";
  const d = new Date(t);
  if (isNaN(d) || d.getFullYear() < 2000) return "-";
  const s = (d.getTime() - Date.now()) / 1000;
  return s <= 0 ? "due" : "in " + span(s);
}

function span(s) {
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

// cachePct is the share of the whole input served from the prompt cache.
// Since caching shipped tokens_in is only the uncached remainder.
function cachePct(inTok, read, write) {
  const total = (inTok || 0) + (read || 0) + (write || 0);
  if (!read || !total) return "-";
  return Math.round((100 * read) / total) + "%";
}

function sourceLabel(src) {
  return src === "subagent" ? "sub" : (src || "chat");
}

function shortID(id) {
  return (id || "").slice(0, 8);
}

// ---------- sessions and live state ----------

function rec(id) {
  let r = S.sessions.get(id);
  if (!r) {
    r = { id, title: "", workspace: "", source: "", parent_id: "", updated_at: "", pending: 0, working: false, approvals: [] };
    S.sessions.set(id, r);
  }
  return r;
}

// stateOf mirrors the TUI: a child's approval blocks its root, never the
// child. The feed publishes a child's ask to its root already; the listed
// pending count credits every ancestor and the child itself, so only a root
// reads it.
function stateOf(r) {
  if (!r.parent_id && (r.approvals.length > 0 || r.pending > 0)) return "blocked";
  if (r.working) return "working";
  return "idle";
}

const GLYPH = { working: "●", blocked: "◐", idle: "○" };

async function loadSessions() {
  const list = await api("GET", "/api/sessions?children=1", {});
  const seen = new Set();
  for (const s of list || []) {
    const r = rec(s.id);
    Object.assign(r, {
      title: s.title, workspace: s.workspace, source: s.source, parent_id: s.parent_id || "",
      updated_at: s.updated_at, pending: s.pending || 0,
      // Only a job run is "unread", as in the TUI: nobody watched it happen.
      unread: !!s.unread && s.source === "job",
      working: s.state === "working" || s.state === "blocked",
    });
    seen.add(s.id);
  }
  for (const id of [...S.sessions.keys()]) {
    if (!seen.has(id)) S.sessions.delete(id);
  }
  renderSidebar();
  renderHeader();
  return list || [];
}

let reloadTimer = null;
function reloadSoon() {
  clearTimeout(reloadTimer);
  reloadTimer = setTimeout(() => loadSessions().catch(() => {}), 400);
}

async function loadJobs() {
  S.jobs = (await api("GET", "/api/jobs", {})) || [];
  const list = el("jobs");
  list.textContent = "";
  const enabled = S.jobs.filter((j) => j.enabled);
  for (const j of enabled) {
    list.appendChild(h("li", { title: j.prompt, text: j.spec + " — " + j.prompt }));
  }
  if (!enabled.length) list.appendChild(h("li", { text: "none" }));
}

// ---------- the global feed ----------

let feed = null;

function connectFeed() {
  if (feed) feed.close();
  feed = new EventSource("/api/events");
  feed.onopen = () => {
    // The hub keeps no backlog and replays pending approvals on every open,
    // so drop what we had and resync, including after a silent reconnect.
    for (const r of S.sessions.values()) r.approvals = [];
    loadSessions().catch((err) => setStatus(err.message, true));
    loadJobs().catch(() => {});
    if (S.open) loadTranscript(S.open, S.generation);
    setStatus("");
  };
  feed.onmessage = (e) => {
    let ev;
    try {
      ev = JSON.parse(e.data);
    } catch (err) {
      setStatus("bad event from the daemon: " + err.message, true);
      return;
    }
    apply(ev);
  };
  feed.onerror = () => {
    // A hard failure closes the stream for good; only a drop on an open
    // stream reconnects on its own.
    if (feed.readyState === EventSource.CLOSED) {
      setStatus("lost connection to the daemon — reload to retry", true);
    } else {
      setStatus("reconnecting…");
    }
  };
}

function dropApprovals(r, pred) {
  const before = r.approvals.length;
  r.approvals = r.approvals.filter((a) => !pred(a));
  return before !== r.approvals.length;
}

function apply(ev) {
  const id = ev.session;
  if (!id) return;
  const isOpen = id === S.open;
  const known = S.sessions.has(id);
  const r = rec(id);
  switch (ev.type) {
    case "turn_started":
      r.working = true;
      r.updated_at = new Date().toISOString();
      break;
    case "text":
      r.working = true;
      if (isOpen) appendText(ev.text);
      break;
    case "tool_call":
      if (isOpen) addToolCall(ev.tool_use_id, ev.tool, ev.args);
      break;
    case "tool_result":
      if (isOpen) addToolResult(ev.tool_use_id, ev.content, ev.is_error, ev.truncated);
      break;
    case "turn_done":
      r.working = false;
      if (isOpen) {
        endLive();
        addFooter(ev);
        S.head.model = ev.model;
        S.head.ctx = (ev.tokens_in || 0) + (ev.tokens_cache_read || 0) + (ev.tokens_cache_write || 0);
        S.head.cost += ev.cost_usd || 0;
      }
      reloadSoon();
      break;
    case "stopped":
      r.working = false;
      dropApprovals(r, (a) => !a.origin_session);
      if (isOpen) { endLive(); addNote("stopped"); setStatus(""); }
      reloadSoon();
      break;
    case "error":
      r.working = false;
      dropApprovals(r, (a) => !a.origin_session);
      if (isOpen) { endLive(); addLine("msg error", "turn failed: " + ev.error); }
      reloadSoon();
      break;
    case "approval":
      if (!r.approvals.some((a) => a.pending_id === ev.pending_id)) r.approvals.push(ev);
      break;
    case "resolved":
      dropApprovals(r, (a) => a.pending_id === ev.pending_id);
      if (!S.answered.has(ev.pending_id) && isOpen) {
        addNote("approval for " + (ev.tool || "a tool") + " answered elsewhere: " + ev.decision);
      }
      S.answered.delete(ev.pending_id);
      r.pending = 0;
      reloadSoon();
      break;
    case "job_note":
      if (isOpen) addNote(ev.text);
      break;
    case "session":
      Object.assign(r, { title: ev.title || r.title, workspace: ev.workspace || r.workspace, source: ev.source || r.source, parent_id: ev.parent_id || r.parent_id });
      // A new session goes to the top; a known one being named stays put.
      if (!known) r.updated_at = new Date().toISOString();
      break;
    case "session_deleted":
      S.sessions.delete(id);
      if (isOpen) {
        S.open = null;
        el("transcript").textContent = "";
        el("approvals").textContent = "";
      }
      break;
    case "agent_state":
      r.working = ev.state === "running";
      if (!r.working) {
        // A settled child can no longer be waiting on anyone.
        for (const other of S.sessions.values()) {
          dropApprovals(other, (a) => a.origin_session === id);
        }
      }
      break;
    default:
      return;
  }
  renderSidebar();
  if (S.open) {
    renderHeader();
    renderApprovals();
    renderThinking();
  }
}

// ---------- sidebar ----------

function renderNav() {
  const nav = el("views");
  nav.textContent = "";
  for (const [key, label, k] of VIEWS) {
    nav.appendChild(h("a", {
      href: "#", class: S.view === key ? "active" : "", "data-view": key,
      onclick: (e) => { e.preventDefault(); openView(key); },
    }, h("span", { text: label }), hint(k)));
  }
}

function matches(r, q) {
  return [r.title, r.id, r.source, r.workspace].some((f) => (f || "").toLowerCase().includes(q));
}

function renderSidebar() {
  const all = [...S.sessions.values()];
  const kids = new Map();
  const roots = [];
  for (const r of all) {
    if (r.parent_id && S.sessions.has(r.parent_id)) {
      if (!kids.has(r.parent_id)) kids.set(r.parent_id, []);
      kids.get(r.parent_id).push(r);
    } else {
      roots.push(r);
    }
  }
  const byUpdated = (a, b) => String(b.updated_at).localeCompare(String(a.updated_at));
  const q = S.filter.trim().toLowerCase();
  // visible reports whether a session or any descendant matches the filter,
  // so a matching child keeps its parent on screen.
  const memo = new Map();
  const visible = (r) => {
    if (!q) return true;
    if (memo.has(r.id)) return memo.get(r.id);
    const v = matches(r, q) || (kids.get(r.id) || []).some(visible);
    memo.set(r.id, v);
    return v;
  };

  const groups = new Map();
  for (const r of roots.filter(visible)) {
    const ws = r.workspace || "(no workspace)";
    if (!groups.has(ws)) groups.set(ws, []);
    groups.get(ws).push(r);
  }
  const ordered = [...groups.entries()].map(([ws, list]) => [ws, list.sort(byUpdated)]);
  ordered.sort((a, b) => byUpdated(a[1][0], b[1][0]));

  const nav = el("sessions");
  nav.textContent = "";
  S.order = [];
  const row = (r, depth) => {
    const st = stateOf(r);
    S.order.push(r.id);
    const a = h("a", {
      href: "#" + r.id, "data-id": r.id,
      class: "srow" + (depth ? " child" : "") + (r.id === S.open ? " active" : "") + (r.unread ? " unread" : ""),
      style: depth > 1 ? "padding-left:" + (12 + 18 * depth) + "px" : null,
      title: r.workspace || "",
      // A click means "show me this conversation", whatever view is open.
      onclick: (e) => { e.preventDefault(); openView("chat"); selectSession(r.id); },
    },
    h("span", { class: "glyph " + st, title: st, text: GLYPH[st] }),
    h("span", { class: "title", text: r.title || shortID(r.id) }),
    h("span", { class: "src", text: sourceLabel(r.source) }));
    nav.appendChild(a);
    for (const c of (kids.get(r.id) || []).filter(visible).sort(byUpdated)) row(c, depth + 1);
  };
  for (const [ws, list] of ordered) {
    nav.appendChild(h("div", { class: "group" }, h("h2", { title: ws, text: tailPath(ws, 34) })));
    for (const r of list) row(r, 0);
  }
  if (!S.order.length) {
    nav.appendChild(h("div", { class: "empty", style: "padding:8px 12px", text: q ? "no session matches" : "no sessions yet" }));
  }

  const blocked = all.filter((r) => stateOf(r) === "blocked").length;
  const banner = el("banner");
  banner.hidden = blocked === 0;
  banner.textContent = "◐ " + blocked + (blocked === 1 ? " session" : " sessions") + " waiting on you";
  banner.appendChild(hint("b"));
}

// ---------- header ----------

function renderHeader() {
  const r = S.open && S.sessions.get(S.open);
  const glyph = el("head-glyph");
  if (!r) {
    glyph.className = "glyph idle";
    glyph.textContent = GLYPH.idle;
    el("head-title").textContent = S.open ? shortID(S.open) : "no session";
    el("head-meta").textContent = "";
    el("head-facts").textContent = "";
    el("stop").hidden = true;
    return;
  }
  const st = stateOf(r);
  glyph.className = "glyph " + st;
  glyph.textContent = GLYPH[st];
  el("head-title").textContent = r.title || shortID(r.id);
  el("head-meta").textContent = [shortID(r.id), sourceLabel(r.source), r.workspace].filter(Boolean).join(" · ");
  el("head-meta").title = r.workspace || "";
  const facts = [];
  if (S.head.model) facts.push(S.head.model);
  if (S.head.ctx) facts.push("ctx " + kTokens(S.head.ctx));
  if (S.head.cost > 0) facts.push(money(S.head.cost));
  if (st === "blocked") facts.push((r.approvals.length || r.pending) + " blocked");
  el("head-facts").textContent = facts.join(" · ");
  el("stop").hidden = !r.working;
}

async function stopTurn() {
  if (!S.open) return;
  try {
    await api("POST", "/api/sessions/{id}/stop", { id: S.open });
    setStatus("stopping…");
  } catch (err) {
    if (err.status === 409) setStatus("nothing running");
    else setStatus(err.message, true);
  }
}

// ---------- transcript ----------

// renderThinking keeps a "spore is thinking" row at the foot of the
// transcript while the open session's turn runs, so a sent message never
// sits there with no sign of life. It hides while an approval waits: then
// spore is waiting on you, and the card says so.
function renderThinking() {
  const t = el("transcript");
  let n = el("thinking");
  const r = S.open && S.sessions.get(S.open);
  if (!r || !r.working || stateOf(r) === "blocked") {
    if (n) n.remove();
    return;
  }
  if (!n) {
    n = h("div", { id: "thinking", class: "thinking", role: "status" },
      h("span", { class: "glyph working", text: "●" }),
      h("span", { class: "label" }),
      h("span", { class: "dots", "aria-hidden": "true" }, h("span", { text: "." }), h("span", { text: "." }), h("span", { text: "." })));
  }
  const pending = [...t.querySelectorAll("details.tool .res.wait")].pop();
  const tool = pending && pending.closest("details.tool")._tool;
  n.querySelector(".label").textContent = tool ? "spore is running " + tool : S.live ? "spore is writing" : "spore is thinking";
  if (t.lastElementChild !== n) {
    t.appendChild(n);
    scrollDown();
  }
}

function scrollDown() {
  const t = el("transcript");
  t.scrollTop = t.scrollHeight;
}

function addLine(cls, text) {
  const n = h("div", { class: cls, text });
  el("transcript").appendChild(n);
  scrollDown();
  return n;
}

function addNote(text) {
  return addLine("msg note", text);
}

function addUser(text) {
  endLive();
  const empty = el("empty-note");
  if (empty) empty.remove();
  el("transcript").appendChild(h("div", { class: "msg user" },
    h("span", { class: "marker", text: "›" }), h("div", { class: "prose", text })));
  scrollDown();
}

function appendText(text) {
  if (!S.live) {
    S.live = h("div", { class: "msg assistant prose" });
    el("transcript").appendChild(S.live);
  }
  S.live.appendChild(document.createTextNode(text || ""));
  scrollDown();
}

function endLive() {
  S.live = null;
}

// argsLine renders a JSON object as key=value pairs on one line.
function argsLine(raw) {
  if (!raw) return "";
  let obj;
  try { obj = typeof raw === "string" ? JSON.parse(raw) : raw; } catch (e) { return cut(String(raw), 80); }
  if (!obj || typeof obj !== "object" || Array.isArray(obj)) return cut(JSON.stringify(obj), 80);
  const parts = Object.entries(obj).map(([k, v]) =>
    k + "=" + (typeof v === "string" ? v.split("\n")[0] : JSON.stringify(v)));
  return cut(parts.join("  "), 80);
}

function argsPretty(raw) {
  if (!raw) return "";
  try { return JSON.stringify(typeof raw === "string" ? JSON.parse(raw) : raw, null, 2); } catch (e) { return String(raw); }
}

function resultSummary(content, isError) {
  const text = (content || "").replace(/\n+$/, "");
  const lines = text ? text.split("\n") : [];
  if (isError) return "✗ " + cut(lines[0] || "error", 60);
  if (lines.length > 1) return "✓ " + lines.length + " lines";
  return "✓ " + (cut(lines[0], 60) || "done");
}

function addToolCall(id, tool, args) {
  endLive();
  const row = h("details", { class: "tool", "data-id": id || "" });
  row._tool = tool;
  row._args = args;
  const sum = h("summary", {},
    h("span", { class: "caret", text: "▸" }),
    h("span", { class: "name", text: tool || "tool" }),
    h("span", { class: "args", text: argsLine(args) }),
    h("span", { class: "res wait", text: "…" }));
  row.appendChild(sum);
  const body = h("div", { class: "body" });
  let code = null;
  if (tool === "go_run") {
    try { code = JSON.parse(args || "{}").code; } catch (e) { code = null; }
  }
  if (typeof code === "string") {
    body.appendChild(h("h4", { text: "program" }));
    body.appendChild(h("pre", { class: "code", text: code }));
  } else {
    body.appendChild(h("h4", { text: "args" }));
    body.appendChild(h("pre", { text: argsPretty(args) }));
  }
  row.appendChild(body);
  row.addEventListener("mouseenter", () => setCursor(row));
  row.addEventListener("toggle", () => setCursor(row));
  el("transcript").appendChild(row);
  if (id) S.tools.set(id, row);
  setCursor(row);
  scrollDown();
  return row;
}

function addToolResult(id, content, isError, truncated) {
  let row = id && S.tools.get(id);
  if (!row) row = addToolCall(id, "tool", "");
  const res = row.querySelector(".res");
  res.className = "res " + (isError ? "err" : "ok");
  res.textContent = resultSummary(content, isError);
  const body = row.querySelector(".body");
  const old = body.querySelector(".result");
  if (old) old.remove();
  const wrap = h("div", { class: "result" },
    h("h4", { text: (isError ? "error" : "result") + (truncated ? " (truncated)" : "") }),
    h("pre", { text: content || "" }));
  body.appendChild(wrap);
  scrollDown();
}

function setCursor(row) {
  if (S.cursor === row) return;
  if (S.cursor) S.cursor.classList.remove("cursor");
  S.cursor = row;
  if (row) row.classList.add("cursor");
}

function footerText(m) {
  const ctx = (m.tokens_in || 0) + (m.tokens_cache_read || 0) + (m.tokens_cache_write || 0);
  const parts = [m.model, "ctx " + kTokens(ctx), kTokens(m.tokens_out) + " out",
    cachePct(m.tokens_in, m.tokens_cache_read, m.tokens_cache_write) + " cached"];
  if (m.cost_usd) parts.push(money(m.cost_usd));
  return parts.join(" · ");
}

function addFooter(m) {
  addLine("footer", footerText(m));
}

function renderTranscript(tr) {
  const t = el("transcript");
  t.textContent = "";
  S.live = null;
  S.tools = new Map();
  S.cursor = null;
  S.head = { model: "", ctx: 0, cost: 0 };
  for (const m of tr.messages || []) {
    if (m.role === "note") {
      for (const b of m.blocks || []) if (b.type === "text") addNote(b.text);
      continue;
    }
    for (const b of m.blocks || []) {
      if (b.type === "text") {
        if (m.role === "user") addUser(b.text);
        else { appendText(b.text); endLive(); }
      } else if (b.type === "tool_use") {
        addToolCall(b.id, b.name, b.input ? JSON.stringify(b.input) : "");
      } else if (b.type === "tool_result") {
        addToolResult(b.id, b.content, b.is_error, b.truncated);
      }
    }
    if (m.model) {
      addFooter(m);
      S.head.model = m.model;
      S.head.ctx = (m.tokens_in || 0) + (m.tokens_cache_read || 0) + (m.tokens_cache_write || 0);
    }
    S.head.cost += m.cost_usd || 0;
  }
  if (!(tr.messages || []).length) addNote("No messages yet. Say something below.").id = "empty-note";
  endLive();
  const r = rec(tr.session.id);
  Object.assign(r, { title: tr.session.title, workspace: tr.session.workspace, source: tr.session.source, parent_id: tr.session.parent_id || "" });
  r.working = r.working || !!tr.running;
  renderHeader();
  renderThinking();
}

async function loadTranscript(id, gen) {
  try {
    const tr = await api("GET", "/api/sessions/{id}", { id });
    // Re-check AFTER the await: the fetch is exactly the window in which
    // the user can switch sessions.
    if (S.generation !== gen) return;
    renderTranscript(tr);
    renderApprovals();
    if (tr.session.unread) {
      api("POST", "/api/sessions/{id}/seen", { id }).then(reloadSoon).catch(() => {});
    }
  } catch (err) {
    if (S.generation !== gen) return;
    setStatus(err.message, true);
  }
}

async function selectSession(id) {
  S.open = id;
  const gen = ++S.generation;
  if (location.hash !== "#" + id) history.replaceState(null, "", "#" + id);
  el("transcript").textContent = "";
  el("approvals").textContent = "";
  S.head = { model: "", ctx: 0, cost: 0 };
  renderSidebar();
  renderHeader();
  if (S.view !== "chat") refreshView();
  await loadTranscript(id, gen);
}

async function newSession() {
  try {
    const s = await api("POST", "/api/sessions", {}, { title: "web" });
    rec(s.id);
    await loadSessions();
    openView("chat");
    await selectSession(s.id);
    el("input").focus();
  } catch (err) {
    setStatus(err.message, true);
  }
}

// ---------- approvals ----------

function renderApprovals() {
  const host = el("approvals");
  const r = S.open && S.sessions.get(S.open);
  const list = r ? r.approvals : [];
  const want = new Set(list.map((a) => "approval-" + a.pending_id));
  for (const n of [...host.children]) if (!want.has(n.id)) n.remove();
  for (const a of list) {
    if (!document.getElementById("approval-" + a.pending_id)) host.appendChild(approvalCard(a));
  }
  tickCountdowns();
}

function approvalCard(a) {
  const card = h("div", { class: "approval", id: "approval-" + a.pending_id, role: "alert" });
  card._ev = a;
  card.appendChild(h("h3", {}, "spore wants to run ", h("code", { text: a.tool })));
  if (a.origin_session) {
    card.appendChild(h("div", { class: "meta", text: "from sub-agent " + a.origin_session }));
  }
  const why = [];
  if (a.rule) why.push("rule " + a.rule);
  if (a.profile) why.push("profile " + a.profile);
  if (why.length) card.appendChild(h("div", { class: "meta", text: why.join(" · ") }));
  card.appendChild(h("pre", { text: argsPretty(a.args) }));
  card.appendChild(h("div", { class: "countdown", "data-exp": a.expires_at || "" }));

  const buttons = h("div", { class: "buttons" });
  const options = [
    ["Allow once", "y", true, "once", "allow"],
    ["Deny", "n", false, "once", "deny"],
    // "session" approves the TOOL for the rest of the session, not these
    // arguments. The title says so; a vaguer label would understate it.
    ["This session", "s", true, "session", ""],
  ];
  if (a.pattern) options.push(["Allow once + propose " + a.pattern, "p", true, "pattern", ""]);
  for (const [label, key, allow, scope, cls] of options) {
    const b = h("button", {
      type: "button", class: cls, "data-key": key,
      title: scope === "session" ? "allow " + a.tool + " for the rest of this session" : null,
      onclick: () => answer(a, allow, scope, card),
    }, label, hint(key));
    buttons.appendChild(b);
  }
  card.appendChild(buttons);
  if (a.pattern) {
    card.appendChild(h("div", { class: "fine", text: "Propose queues the pattern in Refinements for you to accept; nothing changes until you do." }));
  }
  return card;
}

async function answer(a, allow, scope, card) {
  const buttons = card.querySelectorAll("button");
  for (const b of buttons) b.disabled = true;
  S.answered.add(a.pending_id);
  try {
    await api("POST", "/api/sessions/{id}/approvals/{pending}", { id: S.open, pending: a.pending_id }, { allow, scope });
  } catch (err) {
    S.answered.delete(a.pending_id);
    setStatus("could not answer: " + err.message, true);
    for (const b of buttons) b.disabled = false;
  }
}

function tickCountdowns() {
  for (const n of document.querySelectorAll(".approval .countdown")) {
    const exp = n.dataset.exp;
    if (!exp) {
      n.textContent = "waiting (no timeout)";
      continue;
    }
    const s = Math.round((new Date(exp).getTime() - Date.now()) / 1000);
    if (s <= 0) {
      n.textContent = "auto-denying…";
      continue;
    }
    n.textContent = "auto-denies in " + Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
  }
}

// ---------- composer ----------

// slashCommands answer in the browser instead of reaching the model. Any
// other text starting with "/" is still sent as an ordinary message.
const slashCommands = { "/usage": showUsage };

async function send() {
  const input = el("input");
  const text = input.value.trim();
  if (!text || !S.open) return;
  input.value = "";
  addUser(text);
  const cmd = slashCommands[text];
  if (cmd) {
    try {
      await cmd(S.open);
    } catch (err) {
      addLine("msg error", text + " failed: " + err.message);
    }
    return;
  }
  // Show the turn as started now rather than when turn_started arrives, so
  // the message visibly went somewhere.
  const r = rec(S.open);
  const was = r.working;
  r.working = true;
  renderSidebar();
  renderHeader();
  renderThinking();
  try {
    await api("POST", "/api/sessions/{id}/messages", { id: S.open }, { text });
  } catch (err) {
    r.working = was;
    renderSidebar();
    renderHeader();
    renderThinking();
    setStatus(err.message, true);
  }
}

async function showUsage(sid) {
  const u = await api("GET", "/api/usage?session={id}", { id: sid });
  addLine("msg note usage", usageReport(u));
}

// usageReport mirrors internal/usage in Go, so /usage reads the same on
// every surface. Cost is always shown here, as everywhere else in this UI.
function usageReport(u) {
  const sum = (rows) => (rows || []).reduce((t, r) => ({
    turns: t.turns + r.turns, in: t.in + r.tokens_in, out: t.out + r.tokens_out,
    cw: t.cw + r.tokens_cache_write, cr: t.cr + r.tokens_cache_read, cost: t.cost + r.cost_usd,
  }), { turns: 0, in: 0, out: 0, cw: 0, cr: 0, cost: 0 });
  const commas = (n) => n.toLocaleString("en-US");
  const short = (n) => n >= 1e6 ? +(n / 1e6).toFixed(1) + "M" : n >= 1e3 ? +(n / 1e3).toFixed(1) + "k" : String(n);
  const s = sum(u.session);
  let out = "usage, this session\n  turns: " + s.turns +
    "\n  tokens in: " + commas(s.in) + "  out: " + commas(s.out) + "\n";
  if (s.cr + s.cw > 0) {
    const share = Math.round(s.cr * 100 / (s.in + s.cr + s.cw));
    out += "  cache: " + commas(s.cr) + " read, " + commas(s.cw) + " written (" + share + "% of input)\n";
  }
  out += "  cost: " + money(s.cost) + "\n";
  const d = sum(u.days);
  out += "last 30 days, all sessions: " + d.turns + " turns, " + short(d.in) + " in, " +
    short(d.out) + " out, $" + d.cost.toFixed(2);
  return out;
}

// ---------- views ----------

function openView(key) {
  S.view = key;
  renderNav();
  const chat = key === "chat";
  el("chat").hidden = !chat;
  el("view").hidden = chat;
  clearInterval(S.vtimer);
  S.vtimer = null;
  if (!chat) {
    refreshView();
    S.vtimer = setInterval(() => {
      // Do not rebuild the table under someone typing in its filter.
      if (document.activeElement && document.activeElement.id === "vfilter") return;
      refreshView();
    }, 5000);
  }
}

function vs(key) {
  if (!S.vstate[key]) S.vstate[key] = { sort: null, dir: 1, filter: "", selected: null };
  return S.vstate[key];
}

let viewGen = 0;
async function refreshView() {
  const key = S.view;
  const gen = ++viewGen;
  const def = VIEW_DEFS[key];
  if (!def) return;
  let spec;
  try {
    if (def.scoped && !S.open) {
      spec = { title: def.title, rows: [], columns: def.columns || [], empty: "Open a session first: this view belongs to one." };
    } else {
      spec = await def.load();
    }
  } catch (err) {
    if (gen !== viewGen) return;
    spec = { title: def.title, rows: [], columns: [], empty: "could not load: " + err.message };
  }
  if (gen !== viewGen || S.view !== key) return;
  renderTable(key, spec);
}

// renderTable draws one resource view: a title with the row count, a filter,
// sortable columns, inline row actions, and a detail pane for the selected
// row.
function renderTable(key, spec) {
  const st = vs(key);
  const host = el("view");
  const keepFocus = document.activeElement && document.activeElement.id === "vfilter";
  host.textContent = "";

  const q = st.filter.trim().toLowerCase();
  const cell = (c, row) => {
    const v = c.get(row);
    return v === undefined || v === null ? "" : v;
  };
  let rows = spec.rows.filter((row) => !q || spec.columns.some((c) => String(cell(c, row)).toLowerCase().includes(q)));
  if (st.sort !== null && spec.columns[st.sort]) {
    const c = spec.columns[st.sort];
    const val = c.sort || ((row) => cell(c, row));
    rows = rows.slice().sort((a, b) => {
      const x = val(a), y = val(b);
      const cmp = typeof x === "number" && typeof y === "number" ? x - y : String(x).localeCompare(String(y));
      return cmp * st.dir;
    });
  }

  const filter = h("input", {
    id: "vfilter", type: "search", placeholder: "filter", "aria-label": "filter " + key, value: st.filter,
    oninput: (e) => { st.filter = e.target.value; renderTable(key, spec); },
  });
  host.appendChild(h("div", { class: "view-head" },
    h("h2", {}, spec.title, h("span", { class: "count", text: "(" + rows.length + ")" })),
    h("div", { class: "filter-wrap" }, filter, hint("/"))));
  if (spec.before) host.appendChild(spec.before);
  if (spec.subtitle) host.appendChild(h("div", { class: "view-sub", text: spec.subtitle }));

  if (!rows.length) {
    host.appendChild(h("div", { class: "empty", text: spec.empty || (q ? "nothing matches the filter" : "nothing here") }));
  } else {
    const thead = h("tr", {}, spec.columns.map((c, i) => h("th", {
      class: c.num ? "num" : "", scope: "col",
      "aria-sort": st.sort === i ? (st.dir > 0 ? "ascending" : "descending") : null,
      onclick: () => {
        if (st.sort === i) st.dir = -st.dir; else { st.sort = i; st.dir = 1; }
        renderTable(key, spec);
      },
    }, c.title + (st.sort === i ? (st.dir > 0 ? " ▲" : " ▼") : ""))));
    if (spec.actions) thead.appendChild(h("th", {}));
    const tbody = h("tbody");
    for (const row of rows) {
      const id = spec.rowID ? spec.rowID(row) : null;
      const tr = h("tr", {
        class: (id !== null && id === st.selected ? "selected " : "") + (row._bang ? "bang" : ""),
        onclick: () => {
          if (!spec.detail || id === null) return;
          st.selected = st.selected === id ? null : id;
          renderTable(key, spec);
        },
      });
      for (const c of spec.columns) {
        const v = cell(c, row);
        tr.appendChild(h("td", {
          class: [c.num ? "num" : "", c.flex ? "flex" : "", c.cls ? c.cls(row) : ""].join(" ").trim(),
          title: c.flex ? String(v) : null, text: String(v),
        }));
      }
      if (spec.actions) {
        const td = h("td", { class: "actions" });
        for (const a of spec.actions) {
          if (a.applies && !a.applies(row)) continue;
          td.appendChild(h("button", {
            type: "button",
            onclick: async (e) => {
              e.stopPropagation();
              if (a.confirm && !window.confirm(a.confirm(row))) return;
              e.target.disabled = true;
              try {
                await a.run(row);
                setStatus(a.done ? a.done(row) : "");
              } catch (err) {
                setStatus(err.message, true);
              }
              refreshView();
            },
          }, a.label));
        }
        tr.appendChild(td);
      }
      tbody.appendChild(tr);
    }
    host.appendChild(h("table", { class: "grid" }, h("thead", {}, thead), tbody));
  }
  if (spec.after) host.appendChild(spec.after);

  const sel = st.selected !== null && spec.rows.find((row) => spec.rowID && spec.rowID(row) === st.selected);
  if (sel && spec.detail) {
    const d = h("div", { class: "detail" });
    host.appendChild(d);
    Promise.resolve(spec.detail(sel)).then((node) => { d.textContent = ""; d.appendChild(node); })
      .catch((err) => { d.textContent = err.message; });
  } else if (st.selected !== null && !sel) {
    st.selected = null;
  }
  if (keepFocus) {
    filter.focus();
    filter.setSelectionRange(filter.value.length, filter.value.length);
  }
}

function miniTable(columns, rows) {
  return h("table", { class: "grid" },
    h("thead", {}, h("tr", {}, columns.map((c) => h("th", { class: c.num ? "num" : "", text: c.title })))),
    h("tbody", {}, rows.map((row) => h("tr", {}, columns.map((c) =>
      h("td", { class: c.num ? "num" : "", text: String(c.get(row)) }))))));
}

function usageColumns(withDay) {
  const cols = [];
  if (withDay) cols.push({ title: "DAY", get: (r) => r.day });
  cols.push(
    { title: "MODEL", get: (r) => r.model, flex: true },
    { title: "TURNS", get: (r) => r.turns, num: true },
    { title: "IN", get: (r) => kTokens(r.tokens_in), sort: (r) => r.tokens_in, num: true },
    { title: "OUT", get: (r) => kTokens(r.tokens_out), sort: (r) => r.tokens_out, num: true },
    { title: "CACHE %", get: (r) => cachePct(r.tokens_in, r.tokens_cache_read, r.tokens_cache_write), num: true },
    { title: "COST", get: (r) => money(r.cost_usd), sort: (r) => r.cost_usd, num: true });
  return cols;
}

const VIEW_DEFS = {
  skills: {
    title: "skills", scoped: true,
    async load() {
      const res = await api("GET", "/api/sessions/{id}/skills", { id: S.open });
      const rows = (res.skills || []).slice();
      for (const e of res.errors || []) rows.push({ _bang: true, name: "!", loaded: false, body_tokens: "", description: e });
      return {
        title: "skills",
        rows,
        rowID: (r) => (r._bang ? null : r.name),
        columns: [
          { title: "NAME", get: (r) => r.name },
          { title: "LOADED", get: (r) => (r._bang ? "" : r.loaded ? "yes" : "no") },
          { title: "TOKENS", get: (r) => r.body_tokens, num: true },
          { title: "DESCRIPTION", get: (r) => r.description, flex: true },
        ],
        detail: (r) => h("div", {}, h("h3", { text: r.name }), h("div", { class: "prose", text: r.description })),
        empty: "No skills installed.",
      };
    },
  },
  agents: {
    title: "agents", scoped: true,
    async load() {
      const sid = S.open;
      const res = await api("GET", "/api/sessions/{id}/agents", { id: sid });
      return {
        title: "agents",
        rows: res.agents || [],
        rowID: (r) => r.id,
        columns: [
          { title: "ID", get: (r) => shortID(r.id) },
          { title: "STATE", get: (r) => r.state, cls: (r) => "st-" + r.state },
          { title: "AGE", get: (r) => ago(r.started_at), sort: (r) => -new Date(r.started_at).getTime(), num: true },
          { title: "COST", get: (r) => money(r.cost_usd), sort: (r) => r.cost_usd, num: true },
          { title: "PROMPT", get: (r) => (r.prompt || "").split("\n")[0], flex: true },
        ],
        actions: [{
          label: "Stop",
          applies: (r) => r.state === "running",
          confirm: (r) => "stop sub-agent " + shortID(r.id) + "?",
          run: (r) => api("DELETE", "/api/sessions/{id}/agents/{child}", { id: sid, child: r.id }),
          done: (r) => "stopped sub-agent " + shortID(r.id),
        }],
        detail: (r) => h("div", {},
          h("h3", { text: r.id + " · " + r.state }),
          h("pre", { text: r.prompt || "" }),
          r.result ? h("div", {}, h("div", { class: "view-sub", text: "result" }), h("pre", { text: r.result })) : null,
          r.error ? h("div", {}, h("div", { class: "view-sub", text: "error" }), h("pre", { text: r.error })) : null,
          h("p", {}, h("a", { href: "#" + r.id, onclick: (e) => { e.preventDefault(); openView("chat"); selectSession(r.id); }, text: "open transcript" }))),
        empty: "This session has launched no sub-agents.",
      };
    },
  },
  jobs: {
    title: "jobs",
    async load() {
      const jobs = (await api("GET", "/api/jobs", {})) || [];
      return {
        title: "jobs",
        rows: jobs,
        rowID: (r) => r.id,
        columns: [
          { title: "ID", get: (r) => r.id, num: true },
          { title: "KIND", get: (r) => r.kind },
          { title: "SCHEDULE", get: (r) => r.spec },
          { title: "NEXT", get: (r) => (r.enabled ? until(r.next_run) : "cancelled"), cls: (r) => (r.enabled ? "" : "st-cancelled") },
          { title: "LAST", get: (r) => (r.last_run ? ago(r.last_run) + " ago" : "-") },
          { title: "PROMPT", get: (r) => (r.prompt || "").split("\n")[0], flex: true },
        ],
        actions: [{
          label: "Cancel",
          applies: (r) => r.enabled,
          confirm: (r) => "cancel job " + r.id + "?",
          run: async (r) => { await api("DELETE", "/api/jobs/{id}", { id: r.id }); loadJobs().catch(() => {}); },
          done: (r) => "cancelled job " + r.id,
        }],
        detail: async (r) => {
          const runs = (await api("GET", "/api/jobs/{id}/runs", { id: r.id })) || [];
          return h("div", {},
            h("h3", { text: "job " + r.id + " · " + r.spec }),
            h("pre", { text: r.prompt || "" }),
            h("div", { class: "view-sub", text: "runs (" + runs.length + ")" }),
            runs.length ? miniTable([
              { title: "WHEN", get: (x) => ago(x.session.created_at) + " ago" },
              { title: "STATUS", get: (x) => x.status },
              { title: "SESSION", get: (x) => shortID(x.session.id) },
              { title: "OUTPUT", get: (x) => cut((x.output || "").split("\n")[0], 80) },
            ], runs) : h("div", { class: "empty", text: "not run yet" }));
        },
        empty: "No scheduled jobs.",
      };
    },
  },
  usage: {
    title: "usage",
    async load() {
      const res = S.open
        ? await api("GET", "/api/usage?session={id}", { id: S.open })
        : await api("GET", "/api/usage", {});
      const before = h("div", {});
      if (S.open) {
        const r = S.sessions.get(S.open);
        before.appendChild(h("div", { class: "view-sub", text: "this session · " + ((r && r.title) || shortID(S.open)) }));
        before.appendChild((res.session || []).length
          ? miniTable(usageColumns(false), res.session)
          : h("div", { class: "empty", text: "no usage recorded" }));
      }
      return {
        title: "usage",
        before,
        subtitle: "last 30 days, every session (UTC)",
        rows: res.days || [],
        rowID: null,
        columns: usageColumns(true),
        empty: "No usage in the last 30 days.",
      };
    },
  },
  refinements: {
    title: "refinements",
    async load() {
      const rows = ((await api("GET", "/api/refinements", {})) || []).slice();
      // Proposed first: those are the ones waiting on a human.
      rows.sort((a, b) => (a.status === "proposed" ? 0 : 1) - (b.status === "proposed" ? 0 : 1) ||
        String(b.created_at).localeCompare(String(a.created_at)));
      return {
        title: "refinements",
        rows,
        rowID: (r) => r.id,
        columns: [
          { title: "ID", get: (r) => r.id, num: true },
          { title: "STATUS", get: (r) => r.status, cls: (r) => "st-" + r.status },
          { title: "KIND", get: (r) => r.kind },
          { title: "TARGET", get: (r) => r.target },
          { title: "SESSION", get: (r) => shortID(r.session_id) },
          { title: "RATIONALE", get: (r) => r.rationale, flex: true },
        ],
        actions: [
          {
            label: "Accept",
            applies: (r) => r.status === "proposed",
            run: (r) => api("POST", "/api/refinements/{id}/accept", { id: r.id }),
            done: (r) => "accepted refinement " + r.id,
          },
          {
            label: "Reject",
            applies: (r) => r.status === "proposed",
            confirm: (r) => "reject refinement " + r.id + "?",
            run: (r) => api("POST", "/api/refinements/{id}/reject", { id: r.id }),
            done: (r) => "rejected refinement " + r.id,
          },
          {
            label: "Roll back round",
            applies: (r) => r.status === "applied",
            confirm: (r) => String(r.kind).startsWith("policy.")
              ? "roll back: remove " + r.target + " from your policy?"
              : "roll back every edit in round " + r.round_id + "?",
            // The daemon answers 200 even when an edit could not be undone, and a
            // policy rule that failed to come out is still in force.
            run: async (r) => {
              const res = (await api("POST", "/api/sessions/{id}/refine/rollback", { id: r.session_id }, { round_id: r.round_id })) || {};
              const names = (rows) => (rows || []).map((x) => x.target).join(", ");
              if ((res.failed || []).length) throw new Error("could not roll back " + names(res.failed));
              if (!(res.rolled_back || []).length && (res.stale || []).length) {
                throw new Error("nothing to roll back: " + names(res.stale) + " already changed or revoked");
              }
            },
            done: (r) => String(r.kind).startsWith("policy.") ? "removed " + r.target : "rolled back round " + r.round_id,
          },
        ],
        detail: (r) => String(r.kind).startsWith("policy.")
          ? h("div", {},
              h("h3", { text: "#" + r.id + " · " + r.kind }),
              h("pre", { text: r.target }),
              h("div", { class: "prose", text: r.rationale || "" }),
              h("div", { class: "fine", text: "Accepting writes this rule to the managed block of config.toml and applies it at once; rolling back removes it." }))
          : h("div", {},
              h("h3", { text: "#" + r.id + " · " + r.kind + " · " + r.target + " · round " + r.round_id }),
              h("div", { class: "prose", text: r.rationale || "" }),
              h("div", { class: "cols" },
                h("div", {}, h("div", { class: "view-sub", text: "before" }), h("pre", { text: r.before === null ? "(none)" : r.before })),
                h("div", {}, h("div", { class: "view-sub", text: "after" }), h("pre", { text: r.after === null ? "(none)" : r.after })))),
        empty: "No refinements yet.",
      };
    },
  },
};

// ---------- keyboard ----------

const KEYS_DEFAULT = { enabled: true, hints: true };

// loadKeys and saveKeys are the only storage access. A private window can
// throw on any access; the page then runs on the defaults.
function loadKeys() {
  try {
    const raw = window.localStorage.getItem("spore.keys");
    if (!raw) return { ...KEYS_DEFAULT };
    const v = JSON.parse(raw);
    return {
      enabled: typeof v.enabled === "boolean" ? v.enabled : KEYS_DEFAULT.enabled,
      hints: typeof v.hints === "boolean" ? v.hints : KEYS_DEFAULT.hints,
    };
  } catch (e) {
    return { ...KEYS_DEFAULT };
  }
}

function saveKeys() {
  try {
    window.localStorage.setItem("spore.keys", JSON.stringify(S.keys));
  } catch (e) {
    // Keep the in-memory value; it lasts until the page reloads.
  }
}

function applyKeys() {
  document.body.classList.toggle("hints-off", !S.keys.enabled || !S.keys.hints);
  const en = el("set-enabled");
  const hi = el("set-hints");
  en.setAttribute("aria-checked", String(S.keys.enabled));
  hi.setAttribute("aria-checked", String(S.keys.enabled && S.keys.hints));
  hi.disabled = !S.keys.enabled;
}

const SHORTCUTS = [
  ["C S A J U R", "open Chat, Skills, Agents, Jobs, Usage, Refinements"],
  [",", "settings"],
  ["?", "this list"],
  ["j / k", "next / previous session"],
  ["b", "next blocked session"],
  ["n", "new session (deny, while an approval is shown)"],
  ["/", "filter"],
  ["i", "focus the composer"],
  ["o / O", "toggle the tool row under the cursor / all"],
  ["esc", "close overlay → close detail → clear filter → stop the turn"],
  ["y n s p", "approval: allow once, deny, this session, propose pattern"],
];

function openOverlay(id) {
  for (const o of document.querySelectorAll(".overlay")) o.hidden = o.id !== id;
  const o = el(id);
  const first = o.querySelector("button");
  if (first) first.focus();
}

function openOverlayEl() {
  return [...document.querySelectorAll(".overlay")].find((o) => !o.hidden);
}

function closeOverlays() {
  for (const o of document.querySelectorAll(".overlay")) o.hidden = true;
}

function isField(t) {
  if (!t || !t.tagName) return false;
  const tag = t.tagName.toLowerCase();
  return tag === "input" || tag === "textarea" || tag === "select" || t.isContentEditable || !!t.closest("[contenteditable]");
}

function visibleCard() {
  if (S.view !== "chat") return null;
  const card = el("approvals").querySelector(".approval");
  return card && card.offsetParent !== null ? card : null;
}

function step(delta) {
  if (!S.order.length) return;
  const i = S.order.indexOf(S.open);
  const next = i < 0 ? (delta > 0 ? 0 : S.order.length - 1) : (i + delta + S.order.length) % S.order.length;
  selectSession(S.order[next]);
}

function nextBlocked() {
  const blocked = S.order.filter((id) => stateOf(rec(id)) === "blocked");
  if (!blocked.length) {
    setStatus("nothing is waiting on you");
    return;
  }
  const i = S.order.indexOf(S.open);
  const after = blocked.find((id) => S.order.indexOf(id) > i) || blocked[0];
  openView("chat");
  selectSession(after);
}

function escape() {
  if (openOverlayEl()) {
    closeOverlays();
    return;
  }
  if (S.view !== "chat") {
    const st = vs(S.view);
    if (st.selected !== null) {
      st.selected = null;
      refreshView();
      return;
    }
    if (st.filter) {
      st.filter = "";
      refreshView();
      return;
    }
  }
  if (S.filter) {
    S.filter = "";
    el("filter").value = "";
    renderSidebar();
    return;
  }
  const r = S.open && S.sessions.get(S.open);
  if (r && r.working) stopTurn();
}

function onKey(e) {
  // 1. Off means off: no single-key handling at all.
  if (!S.keys.enabled) return;
  // 2. Leave browser and OS shortcuts alone.
  if (e.ctrlKey || e.metaKey || e.altKey) return;
  // 3. In a text field only esc is ours, and it only leaves the field. This
  // is the safety rule: a y typed mid-sentence must never approve a call.
  if (isField(e.target)) {
    if (e.key === "Escape") {
      e.preventDefault();
      e.target.blur();
    }
    return;
  }
  const overlay = openOverlayEl();
  if (overlay && e.key !== "Escape" && e.key !== "?" && e.key !== ",") return;

  const card = visibleCard();
  if (card && ["y", "n", "s", "p"].includes(e.key)) {
    const b = card.querySelector('button[data-key="' + e.key + '"]');
    if (b && !b.disabled) {
      e.preventDefault();
      b.click();
    }
    return;
  }

  const view = VIEWS.find((v) => v[2] === e.key);
  if (view) {
    e.preventDefault();
    closeOverlays();
    openView(view[0]);
    return;
  }
  switch (e.key) {
    case ",": e.preventDefault(); openOverlay("settings"); break;
    case "?": e.preventDefault(); openOverlay("help"); break;
    case "j": e.preventDefault(); step(1); break;
    case "k": e.preventDefault(); step(-1); break;
    case "b": e.preventDefault(); nextBlocked(); break;
    case "n": e.preventDefault(); newSession(); break;
    case "/": {
      e.preventDefault();
      const f = S.view !== "chat" ? el("vfilter") : null;
      (f || el("filter")).focus();
      break;
    }
    case "i": e.preventDefault(); openView("chat"); el("input").focus(); break;
    case "o":
      e.preventDefault();
      if (S.view === "chat") {
        const row = S.cursor || [...el("transcript").querySelectorAll("details.tool")].pop();
        if (row) { row.open = !row.open; setCursor(row); }
      }
      break;
    case "O": {
      e.preventDefault();
      if (S.view !== "chat") break;
      const rows = [...el("transcript").querySelectorAll("details.tool")];
      const open = rows.some((r) => !r.open);
      for (const r of rows) r.open = open;
      break;
    }
    case "Escape": e.preventDefault(); escape(); break;
    default:
  }
}

// ---------- boot ----------

async function main() {
  S.keys = loadKeys();
  applyKeys();
  renderNav();
  el("addr").textContent = location.host;

  const help = el("help-keys");
  for (const [k, what] of SHORTCUTS) help.appendChild(h("tr", {}, h("td", { text: k }), h("td", { text: what })));

  el("composer").addEventListener("submit", (e) => { e.preventDefault(); send(); });
  el("input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    }
  });
  el("new-session").addEventListener("click", newSession);
  el("stop").addEventListener("click", stopTurn);
  el("banner").addEventListener("click", nextBlocked);
  el("filter").addEventListener("input", (e) => { S.filter = e.target.value; renderSidebar(); });
  el("open-settings").addEventListener("click", () => openOverlay("settings"));
  el("set-enabled").addEventListener("click", () => { S.keys.enabled = !S.keys.enabled; saveKeys(); applyKeys(); });
  el("set-hints").addEventListener("click", () => { S.keys.hints = !S.keys.hints; saveKeys(); applyKeys(); });
  for (const o of document.querySelectorAll(".overlay")) {
    o.addEventListener("click", (e) => { if (e.target === o || e.target.closest("[data-close]")) closeOverlays(); });
  }
  document.addEventListener("keydown", onKey);
  // A pasted or bookmarked #<session> link opens that session.
  window.addEventListener("hashchange", () => {
    const id = location.hash.replace("#", "");
    if (id && id !== S.open && S.sessions.has(id)) {
      openView("chat");
      selectSession(id);
    }
  });
  setInterval(tickCountdowns, 1000);

  try {
    const sessions = await loadSessions();
    loadJobs().catch(() => {});
    const wanted = location.hash.replace("#", "");
    const roots = sessions.filter((s) => !s.parent_id);
    const target = (wanted && sessions.some((s) => s.id === wanted) && wanted) || (roots[0] && roots[0].id);
    if (target) await selectSession(target);
  } catch (err) {
    setStatus(err.message, true);
  }
  connectFeed();
}

main();
