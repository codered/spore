"use strict";

// spore's whole front end. No framework, no build step: the daemon serves
// this file straight out of the binary.

let sessionID = null;
let stream = null;
let generation = 0;
// The assistant message currently being streamed, so text deltas append to
// one node instead of creating one per delta.
let liveMessage = null;

const el = (id) => document.getElementById(id);

function setStatus(text, isError) {
  const node = el("status");
  node.textContent = text || "";
  node.className = isError ? "error" : "";
}

async function api(method, path, body) {
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
    const msg = payload && payload.error ? payload.error : res.status + " " + res.statusText;
    throw new Error(msg);
  }
  return payload;
}

// ---------- rendering ----------

function messageNode(role) {
  const wrap = document.createElement("div");
  wrap.className = "msg " + role;
  const label = document.createElement("div");
  label.className = "role";
  label.textContent = role;
  const body = document.createElement("div");
  body.className = "body";
  wrap.appendChild(label);
  wrap.appendChild(body);
  el("transcript").appendChild(wrap);
  scrollDown();
  return wrap;
}

function scrollDown() {
  const t = el("transcript");
  t.scrollTop = t.scrollHeight;
}

function appendToolCall(parent, tool, args) {
  const d = document.createElement("details");
  d.className = "tool";
  const s = document.createElement("summary");
  s.textContent = "→ " + tool;
  const pre = document.createElement("pre");
  pre.textContent = args || "";
  d.appendChild(s);
  d.appendChild(pre);
  parent.appendChild(d);
  scrollDown();
  return d;
}

function appendToolResult(parent, content, isError, truncated) {
  const d = document.createElement("details");
  d.className = isError ? "tool error" : "tool";
  const s = document.createElement("summary");
  s.textContent = (isError ? "← error" : "← result") +
    " (" + (content || "").length + " bytes" + (truncated ? ", truncated" : "") + ")";
  const pre = document.createElement("pre");
  pre.textContent = content || "";
  d.appendChild(s);
  d.appendChild(pre);
  parent.appendChild(d);
  scrollDown();
}

function renderTranscript(tr) {
  const t = el("transcript");
  t.textContent = "";
  liveMessage = null;
  for (const m of tr.messages) {
    const node = messageNode(m.role);
    const body = node.querySelector(".body");
    for (const b of m.blocks) {
      if (b.type === "text") {
        body.appendChild(document.createTextNode(b.text || ""));
      } else if (b.type === "tool_use") {
        appendToolCall(body, b.name, b.input ? JSON.stringify(b.input) : "");
      } else if (b.type === "tool_result") {
        appendToolResult(body, b.content, b.is_error, b.truncated);
      }
    }
    if (m.model) {
      const f = document.createElement("div");
      f.className = "footer";
      f.textContent = m.model + " · " + m.tokens_in + " in / " + m.tokens_out + " out" +
        (m.cost_usd ? " · $" + m.cost_usd.toFixed(4) : "");
      node.appendChild(f);
    }
  }
  setStatus(tr.running ? "a turn is running…" : "");
}

// ---------- approvals ----------

function renderApproval(ev) {
  if (document.getElementById("approval-" + ev.pending_id)) return;
  const box = document.createElement("div");
  box.className = "approval";
  box.id = "approval-" + ev.pending_id;

  const h = document.createElement("h3");
  h.textContent = "spore wants to run " + ev.tool;
  box.appendChild(h);

  if (ev.rule) {
    const why = document.createElement("div");
    why.textContent = "matched policy rule " + ev.rule;
    box.appendChild(why);
  }
  const pre = document.createElement("pre");
  pre.textContent = ev.args || "";
  box.appendChild(pre);

  const buttons = document.createElement("div");
  buttons.className = "buttons";
  const options = [
    ["allow once", true, "once"],
    ["deny", false, "once"],
    // "session" approves the TOOL for the rest of the session, not these
    // arguments. Say so on the button; a vaguer label would understate it.
    ["allow " + ev.tool + " for this session", true, "session"],
  ];
  if (ev.pattern) {
    options.push(["always allow " + ev.pattern, true, "pattern"]);
  }
  for (const [label, allow, scope] of options) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.addEventListener("click", async () => {
      for (const other of buttons.querySelectorAll("button")) other.disabled = true;
      try {
        await api("POST", "/api/sessions/" + sessionID + "/approvals/" + ev.pending_id,
          { allow: allow, scope: scope });
      } catch (err) {
        setStatus("could not answer: " + err.message, true);
        for (const other of buttons.querySelectorAll("button")) other.disabled = false;
      }
    });
    buttons.appendChild(b);
  }
  box.appendChild(buttons);
  el("approvals").appendChild(box);
}

function clearApproval(pendingID) {
  const node = document.getElementById("approval-" + pendingID);
  if (node) node.remove();
}

// ---------- streaming ----------

function handleEvent(ev) {
  switch (ev.type) {
    case "text":
      if (!liveMessage) liveMessage = messageNode("assistant");
      liveMessage.querySelector(".body").appendChild(document.createTextNode(ev.text));
      scrollDown();
      break;
    case "tool_call":
      if (!liveMessage) liveMessage = messageNode("assistant");
      appendToolCall(liveMessage.querySelector(".body"), ev.tool, ev.args);
      break;
    case "tool_result":
      if (!liveMessage) liveMessage = messageNode("assistant");
      appendToolResult(liveMessage.querySelector(".body"), ev.content, ev.is_error, ev.truncated);
      break;
    case "turn_done": {
      const target = liveMessage || messageNode("assistant");
      const f = document.createElement("div");
      f.className = "footer";
      f.textContent = ev.model + " · " + (ev.tokens_in || 0) + " in / " + (ev.tokens_out || 0) + " out" +
        (ev.cost_usd ? " · $" + ev.cost_usd.toFixed(4) : "");
      target.appendChild(f);
      liveMessage = null;
      setStatus("");
      loadSessions();
      break;
    }
    case "error":
      setStatus(ev.error, true);
      liveMessage = null;
      break;
    case "approval":
      renderApproval(ev);
      break;
    case "resolved":
      clearApproval(ev.pending_id);
      setStatus("");
      break;
  }
}

function attach(id, onReady) {
  if (stream) stream.close();
  stream = new EventSource("/api/sessions/" + id + "/events");
  stream.onopen = () => {
    // Re-fetch on every open, including the browser's own silent reconnects:
    // the hub keeps no backlog, so anything published while we were away is
    // not replayed and the transcript would otherwise be quietly incomplete.
    onReady();
  };
  stream.onmessage = (e) => {
    try {
      handleEvent(JSON.parse(e.data));
    } catch (err) {
      setStatus("bad event from the daemon: " + err.message, true);
    }
  };
  stream.onerror = () => {
    // A hard failure (bad status, wrong content-type) closes the stream
    // permanently — the browser will not retry, so saying "reconnecting"
    // would be a lie. Only a drop on an open stream auto-reconnects.
    if (stream && stream.readyState === EventSource.CLOSED) {
      setStatus("lost connection to this session — reload to retry", true);
    } else {
      setStatus("reconnecting…");
    }
  };
}

async function openSession(id) {
  sessionID = id;
  el("approvals").textContent = "";
  // Monotonically increasing generation per openSession call; guards against
  // out-of-order fetches if the user rapidly switches sessions.
  const gen = ++generation;
  const loadTranscript = async () => {
    try {
      const tr = await api("GET", "/api/sessions/" + id);
      // Re-check AFTER the await: the fetch is exactly the window in which
      // the user can switch sessions, so a check before it guards nothing.
      if (generation !== gen) return;
      renderTranscript(tr);
      // Which directory a session operates on is not a detail: two sessions in
      // two projects look identical without it.
      el("workspace").textContent = (tr.session && tr.session.workspace) || "";
    } catch (err) {
      if (generation !== gen) return;   // a stale error is noise too
      setStatus(err.message, true);
    }
  };
  // Load the transcript directly on page load and on hard errors, so the view
  // works whether or not the stream ever opens.
  attach(id, loadTranscript);
  await loadTranscript();
  for (const a of document.querySelectorAll("#sessions a")) {
    a.classList.toggle("active", a.dataset.id === id);
  }
}

// ---------- sidebar ----------

async function loadSessions() {
  const sessions = await api("GET", "/api/sessions");
  const nav = el("sessions");
  nav.textContent = "";
  for (const s of sessions || []) {
    const a = document.createElement("a");
    a.href = "#" + s.id;
    a.dataset.id = s.id;
    a.textContent = s.title || s.id;
    a.classList.toggle("active", s.id === sessionID);
    a.title = s.workspace || "";
    a.addEventListener("click", (e) => {
      e.preventDefault();
      openSession(s.id).catch((err) => setStatus(err.message, true));
    });
    nav.appendChild(a);
  }
  return sessions || [];
}

async function loadJobs() {
  const jobs = await api("GET", "/api/jobs");
  const list = el("jobs");
  list.textContent = "";
  for (const j of jobs || []) {
    const li = document.createElement("li");
    if (!j.enabled) li.className = "cancelled";
    li.textContent = j.spec + " — " + j.prompt;
    list.appendChild(li);
  }
}

// ---------- composer ----------

async function send() {
  const input = el("input");
  const text = input.value.trim();
  if (!text || !sessionID) return;
  input.value = "";
  const node = messageNode("user");
  node.querySelector(".body").textContent = text;
  setStatus("thinking…");
  try {
    await api("POST", "/api/sessions/" + sessionID + "/messages", { text: text });
  } catch (err) {
    setStatus(err.message, true);
  }
}

async function main() {
  el("composer").addEventListener("submit", (e) => {
    e.preventDefault();
    send();
  });
  el("input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
  el("new-session").addEventListener("click", async () => {
    try {
      const s = await api("POST", "/api/sessions", { title: "web" });
      await loadSessions();
      await openSession(s.id);
    } catch (err) {
      setStatus(err.message, true);
    }
  });

  try {
    const sessions = await loadSessions();
    await loadJobs();
    const wanted = location.hash.replace("#", "");
    const target = wanted || (sessions[0] && sessions[0].id);
    if (target) await openSession(target);
  } catch (err) {
    setStatus(err.message, true);
  }
}

main();
