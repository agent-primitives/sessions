// app.js: the page. Every value from the bridge is put in the DOM as text, never as HTML.
(function () {
  "use strict";
  const Flow = window.Flow;
  const $ = (id) => document.getElementById(id);
  const SVG = "http://www.w3.org/2000/svg";

  const state = {
    config: null,
    sessions: [],
    activity: [],
    selected: "",
    journal: null,
    lastSeqSeen: {},
    forkSeq: 0,
    shown: null, // the activity whose flow is drawn
    traces: new Map(),
    busy: false,
    pending: null, // the exec streaming right now, before its activity arrives
  };

  const opNames = { create: "Create", exec: "Exec", suspend: "Suspend", resume: "Resume", fork: "Fork", replay: "Replay" };

  // ---- bridge calls ----

  async function getJSON(path) {
    const res = await fetch(path, { headers: { Accept: "application/json" } });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || res.status + " " + res.statusText);
    return body;
  }

  async function postJSON(path, body) {
    const res = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    const out = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(out.error || res.status + " " + res.statusText);
    return out;
  }

  function setStatus(text, error) {
    const el = $("status");
    el.textContent = text || "";
    el.classList.toggle("error", Boolean(error));
  }

  // ---- small DOM helpers ----

  function el(tag, attrs, ...children) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else n.setAttribute(k, v === true ? "" : v);
    }
    for (const c of children) if (c !== null && c !== undefined) n.append(c);
    return n;
  }

  function svg(tag, attrs) {
    const n = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) n.setAttribute(k, v);
    return n;
  }

  const short = (uid) => (uid ? uid.slice(0, 8) : "");
  const laneOf = (name) => (state.config ? state.config.lanes.find((l) => l.name === name) : null);
  const sessionByID = (uid) => state.sessions.find((s) => s.uid === uid);

  function clock(iso) {
    const d = new Date(iso);
    return isNaN(d) ? "" : d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  // ---- theme ----

  function applyTheme(theme) {
    document.documentElement.dataset.theme = theme;
    const other = theme === "dark" ? "light" : "dark";
    $("theme").textContent = other === "dark" ? "Dark theme" : "Light theme";
    $("theme").setAttribute("aria-label", "Switch to the " + other + " theme");
  }

  function initTheme() {
    const fromURL = new URLSearchParams(location.search).get("theme");
    const saved = localStorage.getItem("lifecycle-theme");
    const os = matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
    applyTheme(fromURL === "dark" || fromURL === "light" ? fromURL : saved || os);
    $("theme").addEventListener("click", () => {
      const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
      localStorage.setItem("lifecycle-theme", next);
      applyTheme(next);
    });
  }

  // ---- flow diagram ----

  function buildFlow() {
    const root = $("flow");
    root.setAttribute("viewBox", `-2 -2 ${Flow.VIEW.width + 4} ${Flow.VIEW.height + 4}`);
    for (const e of Flow.edges) {
      const g = svg("g", { class: "edge off", "data-id": e.id });
      g.append(svg("path", { d: Flow.edgePath(e.id) }));
      const at = Flow.labelAt(e.id);
      const t = svg("text", { x: at.x, y: at.y, "text-anchor": at.anchor });
      t.textContent = e.label;
      g.append(t);
      root.append(g);
    }
    for (const n of Flow.nodes) {
      const g = svg("g", { class: "node off", "data-id": n.id, tabindex: "0", role: "button" });
      g.append(svg("rect", { x: n.x, y: n.y, width: Flow.NODE_W, height: Flow.NODE_H, rx: 7 }));
      const l = svg("text", { class: "label", x: n.x + 12, y: n.y + 23 });
      l.textContent = n.label;
      const s = svg("text", { class: "subl", x: n.x + 12, y: n.y + 41 });
      s.textContent = n.sub;
      g.append(l, s);
      g.addEventListener("click", () => showEvidence(n.id));
      g.addEventListener("focus", () => showEvidence(n.id));
      g.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          showEvidence(n.id);
        }
      });
      root.append(g);
    }
  }

  function nodeSub(id, act) {
    const lane = act ? laneOf(act.lane) : null;
    if (id === "daemon" && lane) return { stateless: "stateless tier", memory: "memory tier", volumes: "VOLUMES template" }[lane.name] || lane.name;
    if (id === "actor" && act && (act.child || act.session)) return "actor " + short(act.child || act.session);
    if (id === "model" && act && act.op === "replay" && act.ok) return "served from the journal";
    return Flow.nodes.find((n) => n.id === id).sub;
  }

  function paintFlow(op, trace, act, animStep) {
    const ns = Flow.nodeStates(op, trace);
    const es = Flow.edgeStates(op, trace);
    const path = Flow.pathFor(op);
    const reached = new Set(["browser"]);
    if (animStep !== undefined) {
      path.slice(0, animStep + 1).forEach((id) => reached.add(Flow.edges.find((e) => e.id === id).to));
    }
    for (const g of $("flow").querySelectorAll(".node")) {
      const id = g.dataset.id;
      let s = ns[id];
      if (animStep !== undefined && s === "pending") s = reached.has(id) ? "active" : "pending";
      g.setAttribute("class", "node " + s + (state.selectedNode === id ? " selected" : ""));
      g.querySelector(".subl").textContent = nodeSub(id, act);
      const ev = trace && trace.nodes ? trace.nodes[id] : null;
      const label = Flow.nodes.find((n) => n.id === id).label;
      g.setAttribute("aria-label", label + ": " + s + (ev && ev.detail ? ". " + ev.detail : ""));
    }
    for (const g of $("flow").querySelectorAll(".edge")) {
      const id = g.dataset.id;
      let s = es[id];
      if (animStep !== undefined && s === "pending") s = path.indexOf(id) <= animStep ? "active" : "pending";
      // The daemon's error marks the call into it; a failure in the bridge leaves it as traced.
      if (act && !act.ok && trace && id === "grpc" && Flow.failedNode(act) === "daemon") {
        s = "failed";
      }
      g.setAttribute("class", "edge " + s);
      g.querySelector("text").textContent = Flow.edgeLabel(op, id, trace, s);
    }
  }

  function caption(act) {
    if (!act) return "Make a call to see the path it takes.";
    const parts = [opNames[act.op] || act.op];
    if (act.session) parts.push("session " + short(act.session));
    if (act.child) parts.push("child " + short(act.child));
    const lane = laneOf(act.lane);
    if (lane) parts.push(lane.daemon);
    if (act.durationMs !== undefined && act.started) parts.push((act.ok ? "OK" : act.code) + " in " + act.durationMs + " ms");
    return parts.join(" · ");
  }

  let animTimer = null;
  function animate(op, act) {
    clearInterval(animTimer);
    let step = 0;
    const path = Flow.pathFor(op);
    $("flow-caption").textContent = (opNames[op] || op) + " in flight…";
    paintFlow(op, null, act, 0);
    animTimer = setInterval(() => {
      step = Math.min(step + 1, path.length - 1);
      paintFlow(op, null, act, step);
    }, 220);
  }

  async function showFlow(act) {
    clearInterval(animTimer);
    state.shown = act;
    state.selectedNode = null;
    $("flow-caption").textContent = caption(act);
    $("evidence").textContent = "";
    if (!act) return paintFlow("create", { nodes: {} }, null);
    paintFlow(act.op, null, act);
    $("evidence").textContent = "Reading the components' logs for this call…";
    let trace = state.traces.get(act.id);
    try {
      if (!trace) {
        trace = await getJSON("/api/trace?id=" + act.id);
        state.traces.set(act.id, trace);
      }
    } catch (err) {
      $("evidence").textContent = "Trace failed: " + err.message;
      return;
    }
    if (state.shown !== act) return;
    paintFlow(act.op, trace, act);
    const focus = !act.ok ? Flow.failedNode(act)
      : act.op === "replay" ? "model" : Flow.nodesOn(act.op).slice(-1)[0];
    showEvidence(focus);
    renderTimeline();
  }

  function showEvidence(id) {
    const act = state.shown;
    state.selectedNode = id;
    const out = $("evidence");
    out.replaceChildren();
    for (const g of $("flow").querySelectorAll(".node")) g.classList.toggle("selected", g.dataset.id === id);
    if (!act) return;
    const trace = state.traces.get(act.id);
    const ev = trace && trace.nodes ? trace.nodes[id] : null;
    const label = Flow.nodes.find((n) => n.id === id).label;
    out.append(el("strong", { text: label + ": " }));
    if (!ev) {
      out.append(document.createTextNode(Flow.nodesOn(act.op).includes(id) ? "no trace yet" : "not on this call's path"));
      return;
    }
    // The diagram's state: a skipped hop the call passed through reads "forwarded", as drawn.
    out.append(document.createTextNode(Flow.nodeStates(act.op, trace)[id] + (ev.detail ? " · " : "")));
    if (ev.detail) out.append(el("code", { text: ev.detail }));
    if (ev.source) out.title = ev.source;
  }

  // ---- lanes and sessions ----

  function renderLanes() {
    const ul = $("lanes");
    ul.replaceChildren();
    const sel = $("lane");
    sel.replaceChildren();
    for (const l of state.config.lanes) {
      ul.append(el("li", { class: l.up ? "up" : "down", title: l.error || l.addr }, el("span", { class: "dot" }), l.daemon + (l.up ? "" : " unreachable")));
      sel.append(el("option", { value: l.name, text: l.label + " (" + l.harness + ")", disabled: !l.up }));
    }
  }

  function sessionLabel(s) {
    const lane = laneOf(s.lane);
    const name = s.name || (s.parentUid ? "fork of " + short(s.parentUid) + " at seq " + s.forkSeq : "unnamed");
    return name + " · " + short(s.uid) + " · " + (lane ? lane.label.toLowerCase() : s.lane);
  }

  function renderSessions() {
    const sel = $("session");
    sel.replaceChildren();
    if (state.sessions.length === 0) sel.append(el("option", { value: "", text: "No session yet" }));
    for (const s of state.sessions) sel.append(el("option", { value: s.uid, text: sessionLabel(s) }));
    sel.value = state.selected || "";
  }

  async function loadSessions() {
    const out = await getJSON("/api/sessions");
    state.sessions = (out.sessions || []).sort((a, b) => (a.created || "").localeCompare(b.created || ""));
    renderSessions();
  }

  function upsertSession(v) {
    if (!v || !v.uid) return;
    const i = state.sessions.findIndex((s) => s.uid === v.uid);
    if (i >= 0) state.sessions[i] = v;
    else state.sessions.push(v);
    renderSessions();
  }

  // ---- selection ----

  async function select(uid) {
    // While a call is in flight its result is drawn beside the session it was made on, so the
    // selection stays put (the session picker is disabled; this covers a fork's "Open the child").
    if (state.busy && uid !== state.selected) return;
    state.selected = uid;
    state.forkSeq = 0;
    $("session").value = uid;
    const url = new URL(location.href);
    if (uid) url.searchParams.set("session", uid);
    else url.searchParams.delete("session");
    history.replaceState(null, "", url);
    renderControls();
    renderTimeline();
    await Promise.all([loadJournal(), loadActor()]);
  }

  function renderControls() {
    const s = sessionByID(state.selected);
    const has = Boolean(s);
    const busy = state.busy;
    const lane = s ? laneOf(s.lane) : null;
    $("suspend").disabled = !has || busy;
    $("resume").disabled = !has || busy;
    $("fork").disabled = !has || busy;
    const block = Flow.replayBlock(s, lane, state.config.replay);
    $("replay").disabled = busy || block !== "";
    $("replay").title = has ? block : "";
    $("session").disabled = busy;
    $("fork").textContent = state.forkSeq > 0 ? "Fork at seq " + state.forkSeq : "Fork at head";
    $("create").disabled = busy;
    $("input").disabled = !has || busy;
    $("send").disabled = !has || busy;
    $("input").placeholder = has ? "Type a turn for " + (s.name || short(s.uid)) + " and press Enter" : "Select or create a session first";
    $("actor-refresh").disabled = !has;
  }

  // ---- timeline ----

  function entryFor(a) {
    const li = el("li", { class: "entry" + (a.ok === false ? " failed" : "") + (state.shown && state.shown.id === a.id ? " current" : ""), tabindex: "0", "data-id": String(a.id) });
    const chip = a.running
      ? el("span", { class: "chip run", text: "running" })
      : el("span", { class: "chip " + (a.ok ? "ok" : "bad"), text: a.ok ? "OK" : a.code || "failed" });
    const head = el("div", { class: "row" }, el("span", { class: "op", text: opNames[a.op] || a.op }), chip);
    if (a.durationMs !== undefined && !a.running) head.append(el("span", { class: "chip", text: a.durationMs + " ms" }));
    head.append(el("span", { class: "when", text: clock(a.started) }));
    li.append(head);

    if (a.op === "exec") {
      li.append(el("p", { class: "line" }, el("span", { class: "who", text: "user" }), a.input || ""));
      const s = sessionByID(a.session);
      if (a.output || a.running) {
        li.append(el("p", { class: "line out" }, el("span", { class: "who", text: (s && s.harness) || "harness" }), a.output || "…"));
      }
      if (a.records && a.records.length) {
        const first = a.records[0].seq;
        const last = a.records[a.records.length - 1].seq;
        li.append(el("p", { class: "note", text: a.records.length + " records streamed, seq " + first + " to " + last }));
      }
    }
    if (a.op === "create" && a.ok) li.append(el("p", { class: "note", text: "Session " + short(a.session) + " on " + ((laneOf(a.lane) || {}).daemon || a.lane) + "; no actor until the first turn." }));
    if (a.op === "fork" && a.ok) {
      const at = a.atSeq ? "seq " + a.atSeq : "the head";
      li.append(el("p", { class: "note" }, "Child ", el("code", { text: short(a.child) }), " from " + short(a.session) + " at " + at + ". ", el("a", { href: "?session=" + encodeURIComponent(a.child), onclick: (ev) => { ev.preventDefault(); ev.stopPropagation(); select(a.child); }, text: "Open the child" })));
    }
    if ((a.op === "suspend" || a.op === "resume") && a.ok && a.after) {
      li.append(el("p", { class: "note", text: "Compute " + a.after.computeState.replace("COMPUTE_", "").toLowerCase() + ", last seq " + a.after.lastSeq + "." }));
    }
    if (a.op === "replay" && a.replay) {
      li.append(el("p", { class: "note", text: Flow.replayNote(a) }));
    }
    if (!a.ok && !a.running) {
      if (a.refusal) {
        const card = el("div", { class: "refusal", role: "group", "aria-label": "Refusal" });
        card.append(el("h3", { text: (opNames[a.refusal.op] || opNames[a.op]) + " refused: snapshot fidelity" }));
        const dl = el("dl");
        for (const [k, v] of Flow.refusalRows(a.refusal)) {
          dl.append(el("dt", { text: k }), el("dd", { class: k.endsWith("fidelity") ? "fid" : "", text: v }));
        }
        card.append(dl);
        const notes = [];
        if (a.refusal.suspended) notes.push("The actor remains suspended.");
        if (a.refusal.memoryLost) notes.push((a.refusal.observed ? "A " + a.refusal.observed + " snapshot" : "This snapshot") + " cannot restore the harness's in-RAM conversation.");
        if (a.after) notes.push("Session last seq after the call: " + a.after.lastSeq + ".");
        if (notes.length) card.append(el("p", { text: notes.join(" ") }));
        li.append(card);
      }
      li.append(el("p", { class: "msg", text: (a.code ? a.code + ": " : "") + (a.message || "") }));
    }
    if (!a.running) {
      li.addEventListener("click", () => showFlow(a));
      li.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter") showFlow(a);
      });
    }
    return li;
  }

  function renderTimeline() {
    const ol = $("timeline");
    // Rebuilding drops the entry that has keyboard focus; it gets focus back below.
    const focused = ol.contains(document.activeElement) ? document.activeElement.dataset.id : undefined;
    ol.replaceChildren();
    const uid = state.selected;
    const acts = state.activity.filter((a) => Flow.concerns(a, uid));
    if (state.pending && state.pending.session === uid) acts.push(state.pending);
    $("timeline-meta").textContent = uid ? acts.length + " call" + (acts.length === 1 ? "" : "s") + " from this page" : "";
    if (!uid) {
      const hasLanes = state.config && state.config.lanes.some((l) => l.up);
      ol.append(
        el("li", { class: "empty" },
          hasLanes
            ? el("span", {}, el("strong", { text: "Create a session to start. " }), "Pick a tier under New on: the stateless tier for exec, suspend, resume, fork and replay; the VOLUMES template for the fidelity refusal.")
            : el("span", {}, el("strong", { text: "No daemon answers. " }), "Run ui.sh after up.sh; it forwards the agentsessionsd Services this page calls.")),
      );
      return;
    }
    if (acts.length === 0) {
      ol.append(el("li", { class: "empty", text: "No calls from this page on this session yet. Send a turn below." }));
      return;
    }
    for (const a of acts) ol.append(entryFor(a));
    if (focused !== undefined) {
      const again = ol.querySelector('.entry[data-id="' + focused + '"]');
      if (again) again.focus({ preventScroll: true });
    }
    // Keep the entry whose flow is drawn in view (its top first, for a tall refusal card); while a
    // turn streams, follow the newest entry.
    const current = ol.querySelector(".entry.current");
    if (current && !state.pending) ol.scrollTop = Math.max(0, current.offsetTop - ol.offsetTop - 8);
    else ol.scrollTop = ol.scrollHeight;
  }

  // ---- actor ----

  async function loadActor() {
    const box = $("actor");
    box.replaceChildren();
    const s = sessionByID(state.selected);
    if (!s) {
      box.append(el("p", { class: "empty", text: "The session's actor appears here once a turn has placed it." }));
      return;
    }
    let out;
    try {
      out = await getJSON("/api/actor?session=" + encodeURIComponent(s.uid));
    } catch (err) {
      if (state.selected === s.uid) box.append(el("p", { class: "empty", text: "Actor read failed: " + err.message }));
      return;
    }
    if (state.selected !== s.uid) return;
    box.replaceChildren();
    const lane = laneOf(s.lane);
    if (!out.available || out.error) {
      const text = !out.available ? out.error : /not ?found/i.test(out.error) ? "No actor yet: CreateSession provisions none; the first turn creates it." : out.error;
      box.append(el("p", { class: "empty", text }));
    } else {
      const a = out.actor;
      const dl = el("dl");
      const add = (k, v) => dl.append(el("dt", { text: k }), el("dd", {}, v));
      add("State", el("span", { class: "state " + a.state, text: a.state }));
      add("Actor", el("code", { text: (a.atespace || state.config.atespace) + "/" + a.name }));
      if (a.uid) add("Actor UID", el("code", { text: a.uid }));
      if (a.worker) add("Worker pod", el("code", { text: a.worker }));
      if (a.template) add("Template", el("code", { text: a.template }));
      box.append(dl);
      box.append(el("h3", { text: "Snapshots (newest first)" }));
      if (!a.snapshots || a.snapshots.length === 0) {
        box.append(el("p", { class: "empty", text: "None yet: a suspend takes one." }));
      } else {
        const t = el("table", { class: "snaps" });
        t.append(el("thead", {}, el("tr", {}, el("th", { text: "gen" }), el("th", { text: "fidelity" }), el("th", { text: "storage" }))));
        const tb = el("tbody");
        for (const sn of a.snapshots.slice(0, 6)) {
          tb.append(el("tr", {}, el("td", { class: "mono", text: String(sn.generation) }), el("td", { class: "fid-" + sn.fidelity, text: sn.fidelity }), el("td", { text: sn.storage.toLowerCase() })));
        }
        t.append(tb);
        box.append(t);
      }
    }
    if (lane) {
      box.append(
        el("p", { class: "declared" },
          "The daemon declares ", el("strong", { text: lane.tier }), " for ", el("code", { text: lane.harness }),
          "; the template prefers ", el("strong", { text: lane.fidelity }), " snapshots."),
      );
    }
  }

  // ---- journal ----

  async function loadJournal() {
    const tb = $("journal").querySelector("tbody");
    const meta = $("journal-meta");
    tb.replaceChildren();
    meta.textContent = "";
    meta.className = "meta";
    const uid = state.selected;
    if (!uid) return;
    let j;
    try {
      j = await getJSON("/api/journal?session=" + encodeURIComponent(uid));
    } catch (err) {
      if (state.selected !== uid) return;
      meta.textContent = "read failed";
      meta.className = "meta bad";
      tb.append(el("tr", {}, el("td", { colspan: "4", class: "text", text: err.message })));
      return;
    }
    if (state.selected !== uid) return;
    state.journal = j;
    const seen = state.lastSeqSeen[uid] || 0;
    const s = sessionByID(uid);
    if (j.records.length === 0) {
      tb.append(el("tr", {}, el("td", { colspan: "4", class: "text", text: "Empty: the first turn writes the first record." })));
    }
    for (const r of j.records) {
      const tr = el("tr", { tabindex: "0", class: (r.seq > seen && seen > 0 ? "fresh " : "") + (r.seq === state.forkSeq ? "fork" : ""), title: "Fork here: seq " + r.seq });
      const content = el("td", { class: "text" });
      if (r.text) content.append((r.detail ? r.detail + ": " : "") + r.text);
      else if (r.detail) content.append(el("span", { class: "detail", text: r.detail }));
      tr.append(el("td", { text: String(r.seq) }), el("td", { class: "kind k-" + r.kind, text: r.kind }), content, el("td", { class: "hash", text: r.hash }));
      const pick = () => {
        state.forkSeq = state.forkSeq === r.seq ? 0 : r.seq;
        renderControls();
        for (const row of tb.children) row.classList.remove("fork");
        if (state.forkSeq) tr.classList.add("fork");
      };
      tr.addEventListener("click", pick);
      tr.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          pick();
        }
      });
      tb.append(tr);
    }
    state.lastSeqSeen[uid] = j.records.length ? j.records[j.records.length - 1].seq : 0;
    const parts = [j.records.length + " records"];
    if (s && s.parentUid) parts.push("forked from " + short(s.parentUid) + " at seq " + s.forkSeq);
    if (j.records.length) parts.push(j.verified ? "hash chain verified here" : "chain does not verify: " + j.error);
    meta.textContent = parts.join(" · ");
    meta.className = "meta " + (j.records.length ? (j.verified ? "ok" : "bad") : "");
    const wrap = tb.closest(".journal-wrap");
    wrap.scrollTop = wrap.scrollHeight;
  }

  // ---- calls ----

  async function run(op, path, body, sessionForAnim) {
    if (state.busy) return null;
    state.busy = true;
    renderControls();
    setStatus("");
    const s = sessionByID(sessionForAnim);
    animate(op, { op, lane: s ? s.lane : body.lane, session: sessionForAnim });
    let act = null;
    try {
      act = await postJSON(path, body);
    } catch (err) {
      setStatus(opNames[op] + " was not sent: " + err.message, true);
    }
    state.busy = false;
    if (act) await settle(act);
    else {
      clearInterval(animTimer);
      showFlow(state.shown);
    }
    renderControls();
    return act;
  }

  // settle records a finished activity and refreshes what it changed.
  async function settle(act) {
    state.activity.push(act);
    if (act.after) upsertSession(act.after);
    if (act.op === "create" && act.ok) {
      await select(act.session);
    } else {
      renderTimeline();
      await Promise.all([loadJournal(), loadActor()]);
    }
    // The controls (Replay needs COMPUTE_LIVE) follow what Sessions reports now, for every session.
    try {
      await loadSessions();
    } catch (err) {
      setStatus("Session list read failed: " + err.message, true);
    }
    renderControls();
    if (!Flow.concerns(act, state.selected)) return;
    const url = new URL(location.href);
    url.searchParams.set("activity", act.id);
    history.replaceState(null, "", url);
    await showFlow(act);
  }

  async function execTurn(text) {
    const uid = state.selected;
    if (!uid || state.busy) return;
    state.busy = true;
    renderControls();
    setStatus("");
    const s = sessionByID(uid);
    animate("exec", { op: "exec", lane: s.lane, session: uid });
    state.pending = { id: -1, op: "exec", session: uid, input: text, output: "", records: [], started: new Date().toISOString(), running: true };
    renderTimeline();
    let act = null;
    try {
      const res = await fetch("/api/exec", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ session: uid, input: text }) });
      if (!res.ok) {
        const e = await res.json().catch(() => ({}));
        throw new Error(e.error || res.statusText);
      }
      const reader = res.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl);
          buf = buf.slice(nl + 1);
          if (!line.trim()) continue;
          const msg = JSON.parse(line);
          if (msg.type === "record") {
            state.pending.records.push(msg.data);
            if (msg.data.kind === "OUTPUT") state.pending.output = msg.data.text;
          } else if (msg.type === "delta") {
            state.pending.output += msg.data;
          } else if (msg.type === "activity") {
            act = msg.data;
          }
          renderTimeline();
        }
      }
    } catch (err) {
      setStatus("Exec failed: " + err.message, true);
    }
    state.pending = null;
    state.busy = false;
    if (act) await settle(act);
    else {
      clearInterval(animTimer);
      renderTimeline();
    }
    renderControls();
  }

  function wire() {
    $("session").addEventListener("change", (ev) => select(ev.target.value));
    $("create").addEventListener("click", () => {
      const lane = $("lane").value;
      if (!lane) return;
      run("create", "/api/create", { lane, name: "" }, "");
    });
    $("suspend").addEventListener("click", () => run("suspend", "/api/suspend", { session: state.selected }, state.selected));
    $("resume").addEventListener("click", () => run("resume", "/api/resume", { session: state.selected }, state.selected));
    $("fork").addEventListener("click", () => run("fork", "/api/fork", { session: state.selected, atSeq: state.forkSeq }, state.selected));
    $("replay").addEventListener("click", () => run("replay", "/api/replay", { session: state.selected }, state.selected));
    $("actor-refresh").addEventListener("click", loadActor);
    $("composer").addEventListener("submit", (ev) => {
      ev.preventDefault();
      const text = $("input").value.trim();
      if (!text) return;
      $("input").value = "";
      execTurn(text).then(() => $("input").focus());
    });
  }

  async function start() {
    initTheme();
    buildFlow();
    wire();
    paintFlow("create", { nodes: {} }, null);
    try {
      state.config = await getJSON("/api/config");
    } catch (err) {
      setStatus("The bridge did not answer: " + err.message, true);
      return;
    }
    renderLanes();
    try {
      await loadSessions();
      state.activity = (await getJSON("/api/activity")).activity || [];
    } catch (err) {
      setStatus(err.message, true);
    }
    const params = new URLSearchParams(location.search);
    const uid = params.get("session") || "";
    await select(sessionByID(uid) ? uid : "");
    const want = Number(params.get("activity"));
    const act = state.activity.find((a) => a.id === want) || state.activity.filter((a) => Flow.concerns(a, state.selected)).slice(-1)[0];
    if (act) await showFlow(act);
    renderTimeline();
  }

  document.addEventListener("DOMContentLoaded", start);
})();
