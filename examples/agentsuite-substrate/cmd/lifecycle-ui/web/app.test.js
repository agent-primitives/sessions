// app.test.js: the page's session selection, run in a small fake DOM (no browser or npm package).
// It checks that the request flow drawn always concerns the selected session.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const Flow = require("./flow.js");

const appSource = fs.readFileSync(path.join(__dirname, "app.js"), "utf8");

// ---- a fake DOM: only what app.js uses ----

class FakeNode {
  constructor(doc, tag) {
    this.ownerDocument = doc;
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.dataset = {};
    this.listeners = {};
    this.ownText = "";
    this.className = "";
    this.value = "";
    this.disabled = false;
    this.scrollTop = 0;
    this.scrollHeight = 0;
    this.offsetTop = 0;
  }
  get classList() {
    const names = () => this.className.split(/\s+/).filter(Boolean);
    return {
      contains: (c) => names().includes(c),
      add: (c) => { if (!names().includes(c)) this.className = names().concat(c).join(" "); },
      remove: (c) => { this.className = names().filter((n) => n !== c).join(" "); },
      toggle: (c, on) => {
        const want = on === undefined ? !names().includes(c) : Boolean(on);
        if (want) this.classList.add(c);
        else this.classList.remove(c);
      },
    };
  }
  setAttribute(k, v) {
    v = String(v);
    this.attrs[k] = v;
    if (k === "class") this.className = v;
    if (k.startsWith("data-")) this.dataset[k.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] = v;
  }
  getAttribute(k) {
    return k === "class" ? this.className : k in this.attrs ? this.attrs[k] : null;
  }
  append(...cs) {
    for (let c of cs) {
      if (typeof c === "string") c = { textContent: c };
      c.parentNode = this;
      this.children.push(c);
    }
  }
  replaceChildren(...cs) {
    this.children = [];
    this.ownText = "";
    this.append(...cs);
  }
  get textContent() {
    return this.ownText + this.children.map((c) => c.textContent).join("");
  }
  set textContent(v) {
    this.children = [];
    this.ownText = String(v);
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  // fire runs the listeners and resolves when the async ones finish.
  fire(type, ev) {
    return Promise.all((this.listeners[type] || []).map((fn) => fn(ev || { target: this, preventDefault() {}, stopPropagation() {} })));
  }
  descendants() {
    const out = [];
    for (const c of this.children) {
      if (!(c instanceof FakeNode)) continue;
      out.push(c, ...c.descendants());
    }
    return out;
  }
  matches(sel) {
    const m = /^([a-z]+)?((?:\.[\w-]+)*)(?:\[data-id="([^"]*)"\])?$/i.exec(sel);
    if (!m) throw new Error("fake DOM: unsupported selector " + sel);
    if (m[1] && this.tagName !== m[1].toUpperCase()) return false;
    for (const c of (m[2] || "").split(".").filter(Boolean)) if (!this.classList.contains(c)) return false;
    return m[3] === undefined || this.dataset.id === m[3];
  }
  querySelectorAll(sel) {
    return this.descendants().filter((n) => n.matches(sel));
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
  contains(n) {
    for (let p = n; p; p = p.parentNode) if (p === this) return true;
    return false;
  }
  closest(sel) {
    for (let p = this; p instanceof FakeNode; p = p.parentNode) if (p.matches(sel)) return p;
    return null;
  }
  focus() {
    this.ownerDocument.activeElement = this;
  }
}

function fakeDocument() {
  const doc = { documentElement: { dataset: {} }, activeElement: null, listeners: {} };
  const ids = new Map();
  doc.createElement = (tag) => new FakeNode(doc, tag);
  doc.createElementNS = (_, tag) => new FakeNode(doc, tag);
  doc.createTextNode = (text) => ({ textContent: String(text) });
  doc.getElementById = (id) => {
    if (!ids.has(id)) ids.set(id, new FakeNode(doc, "div"));
    return ids.get(id);
  };
  doc.addEventListener = (type, fn) => {
    doc.listeners[type] = fn;
  };
  // The journal's tbody sits in its scrolling wrapper, as in index.html.
  const wrap = new FakeNode(doc, "div");
  wrap.className = "journal-wrap";
  const table = doc.getElementById("journal");
  wrap.append(table);
  table.append(new FakeNode(doc, "tbody"));
  return doc;
}

// ---- the bridge, faked ----

const A = "aaaaaaaa-0000-4000-8000-000000000001";
const B = "bbbbbbbb-0000-4000-8000-000000000002";

function session(uid, name) {
  return { uid, name, lane: "stateless", harness: "agentsuite", computeState: "COMPUTE_LIVE", created: uid === A ? "1" : "2" };
}

function activity(id, op, uid) {
  return { id, op, session: uid, lane: "stateless", ok: true, started: "2026-10-21T00:00:00Z", durationMs: 5, records: [] };
}

// page loads app.js against a fake bridge. routes maps a path (without its query) to a body or a
// function of the URL returning one; a returned promise is awaited, so a test can hold a call.
async function page(search, routes, timers) {
  const doc = fakeDocument();
  const loc = { href: "http://127.0.0.1:8090/" + search, get search() { return new URL(this.href).search; } };
  const fetch = async (p) => {
    const u = new URL(p, loc.href);
    const r = routes[u.pathname];
    if (r === undefined) throw new Error("fake bridge: no route " + u.pathname);
    const body = await (typeof r === "function" ? r(u) : r);
    return { ok: true, status: 200, statusText: "OK", json: async () => body };
  };
  const ctx = {
    window: { Flow },
    document: doc,
    location: loc,
    history: { replaceState: (_, __, url) => { loc.href = String(url); } },
    localStorage: { getItem: () => null, setItem() {} },
    matchMedia: () => ({ matches: false }),
    fetch,
    URL,
    URLSearchParams,
    TextDecoder,
    setInterval: () => 1,
    clearInterval() {},
    console,
    ...timers,
  };
  vm.createContext(ctx);
  vm.runInContext(appSource, ctx, { filename: "app.js" });
  await doc.listeners.DOMContentLoaded();
  const $ = (id) => doc.getElementById(id);
  return {
    $,
    loc,
    caption: () => $("flow-caption").textContent,
    activityParam: () => new URL(loc.href).searchParams.get("activity"),
    pick: (uid) => $("session").fire("change", { target: { value: uid } }),
  };
}

function routes(acts, extra) {
  return Object.assign({
    "/api/config": { lanes: [{ name: "stateless", label: "Stateless", daemon: "agentsessionsd-stateless", harness: "agentsuite", tier: "STATELESS_REPLAY", fidelity: "VOLUMES", up: true }], replay: true, atespace: "demo" },
    "/api/sessions": { sessions: [session(A, "a"), session(B, "b")] },
    "/api/activity": { activity: acts },
    "/api/journal": { records: [], verified: true },
    "/api/actor": { available: false, error: "cluster reads are off" },
    "/api/trace": (u) => ({ activity: Number(u.searchParams.get("id")), nodes: { browser: { state: "observed" }, bridge: { state: "observed" } } }),
  }, extra);
}

const flush = () => new Promise((r) => setImmediate(r));

test("selecting another session clears a flow that is not its own", async () => {
  const p = await page("?session=" + A, routes([activity(1, "exec", A)]));
  assert.match(p.caption(), /^Exec · session aaaaaaaa/);
  assert.equal(p.activityParam(), "1");
  await p.pick(B);
  assert.equal(p.caption(), "Make a call to see the path it takes.");
  assert.equal(p.activityParam(), null);
  assert.equal(p.$("timeline").querySelectorAll(".entry").length, 0);
});

test("selecting another session draws that session's latest call", async () => {
  const p = await page("?session=" + A, routes([activity(1, "exec", A), activity(2, "suspend", B), activity(3, "exec", A)]));
  assert.match(p.caption(), /^Exec · session aaaaaaaa/);
  await p.pick(B);
  assert.match(p.caption(), /^Suspend · session bbbbbbbb/);
  assert.equal(p.activityParam(), "2");
  await p.pick(A);
  assert.match(p.caption(), /^Exec · session aaaaaaaa/);
  assert.equal(p.activityParam(), "3");
});

test("a URL's activity of another session is not drawn beside the selected one", async () => {
  const acts = [activity(1, "exec", A), activity(2, "exec", B)];
  let p = await page("?session=" + B + "&activity=1", routes(acts));
  assert.match(p.caption(), /^Exec · session bbbbbbbb/);
  assert.equal(p.activityParam(), "2");
  p = await page("?session=" + B + "&activity=1", routes([activity(1, "exec", A)]));
  assert.equal(p.caption(), "Make a call to see the path it takes.");
  // The session's own call named in the URL is drawn, not only its latest.
  p = await page("?session=" + A + "&activity=1", routes([activity(1, "exec", A), activity(3, "suspend", A)]));
  assert.match(p.caption(), /^Exec · session aaaaaaaa/);
});

test("a call that settles after the user moved to another session is not drawn there", async () => {
  let release;
  const held = new Promise((r) => { release = r; });
  let journalReads = 0;
  const p = await page("?session=" + A, routes([activity(1, "exec", A)], {
    "/api/suspend": activity(2, "suspend", A),
    // Settling the suspend re-reads A's journal; hold that read until the user has picked B.
    "/api/journal": (u) => {
      journalReads++;
      const body = { records: [], verified: true };
      return u.searchParams.get("session") === A && journalReads > 1 ? held.then(() => body) : body;
    },
  }));
  const clicked = p.$("suspend").fire("click");
  for (let i = 0; i < 20 && journalReads < 2; i++) await flush();
  assert.equal(journalReads, 2, "settle is reading A's journal");
  await p.pick(B);
  release();
  await clicked;
  assert.equal(p.caption(), "Make a call to see the path it takes.");
  assert.equal(new URL(p.loc.href).searchParams.get("session"), B);
  assert.equal(p.activityParam(), null);
  // Back on A, the settled call is A's latest and is drawn.
  await p.pick(A);
  assert.match(p.caption(), /^Suspend · session aaaaaaaa/);
});

test("a Create that fails ends its animation and shows the error, with or without a selection", async () => {
  for (const search of ["", "?session=" + A]) {
    let animating = false;
    const failed = { id: 2, op: "create", session: "", lane: "stateless", ok: false, code: "Unavailable", message: "connection refused", started: "2026-10-21T00:00:00Z", durationMs: 3, records: [] };
    const p = await page(search, routes([activity(1, "exec", A)], { "/api/create": failed }), {
      setInterval: () => { animating = true; return 1; },
      clearInterval: () => { animating = false; },
    });
    p.$("lane").value = "stateless";
    // The click handler does not return run's promise; wait for the call to settle.
    await p.$("create").fire("click");
    for (let i = 0; i < 50 && !p.$("status").textContent; i++) await flush();
    await flush();
    assert.equal(animating, false, "the in-flight animation stopped");
    assert.doesNotMatch(p.caption(), /in flight/);
    assert.match(p.caption(), /^Create · agentsessionsd-stateless · Unavailable in 3 ms$/);
    assert.equal(p.$("status").textContent, "Create failed: Unavailable: connection refused");
    assert.ok(p.$("status").classList.contains("error"));
    assert.equal(p.activityParam(), "2");
    // The selection is unchanged, and picking a session again draws that session's own flow.
    assert.equal(new URL(p.loc.href).searchParams.get("session"), search ? A : null);
    await p.pick(A);
    assert.match(p.caption(), /^Exec · session aaaaaaaa/);
  }
});
