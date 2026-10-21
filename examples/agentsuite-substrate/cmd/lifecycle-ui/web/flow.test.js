// Run with: node --test (from this directory). No DOM and no dependencies.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const Flow = require("./flow.js");

const ops = ["create", "exec", "suspend", "resume", "fork", "replay"];

test("every edge connects two known nodes and has a path", () => {
  const ids = new Set(Flow.nodes.map((n) => n.id));
  for (const e of Flow.edges) {
    assert.ok(ids.has(e.from), `${e.id}: unknown from ${e.from}`);
    assert.ok(ids.has(e.to), `${e.id}: unknown to ${e.to}`);
    assert.match(Flow.edgePath(e.id), /^M\d+ \d+( [HV]\d+)+$/, e.id);
  }
});

test("nodes stay inside the view and do not overlap", () => {
  for (const n of Flow.nodes) {
    assert.ok(n.x >= 0 && n.x + Flow.NODE_W <= Flow.VIEW.width, `${n.id} x`);
    assert.ok(n.y >= 0 && n.y + Flow.NODE_H <= Flow.VIEW.height, `${n.id} y`);
  }
  for (const a of Flow.nodes) {
    for (const b of Flow.nodes) {
      if (a === b) continue;
      const apart = a.x + Flow.NODE_W <= b.x || b.x + Flow.NODE_W <= a.x || a.y + Flow.NODE_H <= b.y || b.y + Flow.NODE_H <= a.y;
      assert.ok(apart, `${a.id} overlaps ${b.id}`);
    }
  }
});

// Every segment of every edge, as axis-aligned lines, for the crossing and clearance checks.
function segments(id) {
  const parts = Flow.edgePath(id).match(/[MHV]\d+( \d+)?/g);
  const out = [];
  let x = 0;
  let y = 0;
  for (const p of parts) {
    const nums = p.slice(1).split(" ").map(Number);
    if (p[0] === "M") {
      [x, y] = nums;
      continue;
    }
    const [nx, ny] = p[0] === "H" ? [nums[0], y] : [x, nums[0]];
    out.push({ x1: Math.min(x, nx), x2: Math.max(x, nx), y1: Math.min(y, ny), y2: Math.max(y, ny) });
    [x, y] = [nx, ny];
  }
  return out;
}

test("no edge passes through a node it does not connect", () => {
  for (const e of Flow.edges) {
    for (const n of Flow.nodes) {
      if (n.id === e.from || n.id === e.to) continue;
      for (const s of segments(e.id)) {
        const inside = s.x2 > n.x && s.x1 < n.x + Flow.NODE_W && s.y2 > n.y && s.y1 < n.y + Flow.NODE_H;
        assert.ok(!inside, `${e.id} crosses ${n.id}`);
      }
    }
  }
});

test("no two edges cross", () => {
  for (const a of Flow.edges) {
    for (const b of Flow.edges) {
      if (a.id >= b.id) continue;
      for (const s of segments(a.id)) {
        for (const t of segments(b.id)) {
          const sh = s.y1 === s.y2;
          const th = t.y1 === t.y2;
          if (sh === th) continue;
          const [h, v] = sh ? [s, t] : [t, s];
          const crosses = v.x1 > h.x1 && v.x1 < h.x2 && h.y1 > v.y1 && h.y1 < v.y2;
          assert.ok(!crosses, `${a.id} crosses ${b.id}`);
        }
      }
    }
  }
});

test("each call's path starts at the browser and is connected in order", () => {
  for (const op of ops) {
    const path = Flow.pathFor(op);
    assert.ok(path.length > 0, op);
    const reached = new Set(["browser"]);
    for (const id of path) {
      const e = Flow.edges.find((x) => x.id === id);
      assert.ok(e, `${op}: unknown edge ${id}`);
      assert.ok(reached.has(e.from), `${op}: ${id} starts at ${e.from}, not yet reached`);
      reached.add(e.to);
    }
  }
});

test("create stops at the daemon; replay never takes the control path", () => {
  assert.deepEqual(Flow.nodesOn("create"), ["browser", "bridge", "daemon"]);
  const replay = Flow.pathFor("replay");
  for (const id of ["control", "herd", "model"]) assert.ok(!replay.includes(id), id);
});

test("without a trace, nodes on the path are pending and the rest off", () => {
  const s = Flow.nodeStates("suspend", null);
  assert.equal(s.daemon, "pending");
  assert.equal(s.actor, "pending");
  assert.equal(s.router, "off");
  assert.equal(s.model, "off");
});

test("a trace lights only the hops it observed", () => {
  const trace = {
    nodes: {
      browser: { state: "observed" },
      bridge: { state: "observed" },
      daemon: { state: "observed" },
      ateapi: { state: "observed" },
      worker: { state: "missing" },
      actor: { state: "observed" },
    },
  };
  const es = Flow.edgeStates("resume", trace);
  assert.equal(es.control, "done");
  assert.equal(es.herd, "missing");
  assert.equal(es.wire, "off");
});

test("a refused call marks the daemon and the edge into it done, not missing", () => {
  const trace = { nodes: { browser: { state: "observed" }, bridge: { state: "observed" }, daemon: { state: "refused" } } };
  assert.equal(Flow.nodeStates("suspend", trace).daemon, "refused");
  assert.equal(Flow.edgeStates("suspend", trace).grpc, "done");
});

test("replay shows the model as asked-and-not-taken", () => {
  const trace = { nodes: { model: { state: "skipped", detail: "controller counted 0 model calls" } } };
  assert.equal(Flow.nodeStates("replay", trace).model, "skipped");
  assert.equal(Flow.edgeStates("replay", trace).model, "off");
});

test("an exec that attaches to a running actor lights GetActor and the routed path, not a restore", () => {
  const trace = {
    nodes: {
      browser: { state: "observed" },
      bridge: { state: "observed" },
      daemon: { state: "observed" },
      ateapi: { state: "observed", method: "GetActor" },
      worker: { state: "skipped" },
      actor: { state: "observed" },
      router: { state: "observed", method: "Route ok" },
      model: { state: "observed" },
    },
  };
  const es = Flow.edgeStates("exec", trace);
  assert.equal(es.control, "done");
  assert.equal(Flow.edgeLabel("exec", "control", trace, es.control), "GetActor");
  assert.equal(es.herd, "off");
  assert.equal(Flow.edgeLabel("exec", "herd", trace, es.herd), "atelet");
  assert.equal(es.route, "done");
  assert.equal(es.vm, "done");
  assert.equal(es.wire, "done");
  assert.equal(Flow.nodeStates("exec", trace).worker, "forwarded");
});

test("an exec that places the actor labels the control and atelet edges with the traced methods", () => {
  const trace = {
    nodes: {
      ateapi: { state: "observed", method: "CreateActor + ResumeActor" },
      worker: { state: "observed", method: "RestoreWorkload" },
      router: { state: "missing" },
      actor: { state: "observed" },
    },
  };
  const es = Flow.edgeStates("exec", trace);
  assert.equal(Flow.edgeLabel("exec", "control", trace, es.control), "CreateActor + ResumeActor");
  assert.equal(Flow.edgeLabel("exec", "herd", trace, es.herd), "RestoreWorkload");
  assert.equal(Flow.nodeStates("exec", trace).worker, "observed");
  // The turn's hops rest on the router's line: without it they are not seen.
  assert.equal(es.route, "missing");
  assert.equal(es.vm, "missing");
});

test("no edge claims a method before the trace has one", () => {
  for (const op of ops) {
    for (const id of ["control", "herd"]) {
      for (const s of ["pending", "active", "missing"]) {
        assert.equal(Flow.edgeLabel(op, id, null, s), Flow.edges.find((e) => e.id === id).label, `${op} ${id} ${s}`);
      }
    }
  }
});

test("replay draws a connected path through the worker pod", () => {
  const trace = {
    nodes: {
      daemon: { state: "observed" },
      worker: { state: "skipped" },
      actor: { state: "observed" },
      router: { state: "observed" },
      model: { state: "skipped" },
    },
  };
  const es = Flow.edgeStates("replay", trace);
  assert.equal(es.route, "done");
  assert.equal(es.vm, "done");
  assert.equal(Flow.nodeStates("replay", trace).worker, "forwarded");
});

test("refusal rows keep reading order and drop missing fields", () => {
  assert.deepEqual(Flow.refusalRows(null), []);
  assert.deepEqual(Flow.refusalRows({ session: "s1", actor: "ns/s1", required: "MEMORY", observed: "VOLUMES" }), [
    ["Session", "s1"],
    ["Actor", "ns/s1"],
    ["Required fidelity", "MEMORY"],
    ["Observed fidelity", "VOLUMES"],
  ]);
  assert.deepEqual(Flow.refusalRows({ actor: "ns/s1" }), [["Actor", "ns/s1"]]);
});

test("a resume of a running actor lights the no-op ResumeActor and no restore", () => {
  const trace = {
    nodes: {
      browser: { state: "observed" },
      bridge: { state: "observed" },
      daemon: { state: "observed" },
      ateapi: { state: "observed", method: "GetActor + ResumeActor (already RUNNING)" },
      worker: { state: "skipped" },
      actor: { state: "observed" },
    },
  };
  const es = Flow.edgeStates("resume", trace);
  assert.equal(es.control, "done");
  assert.equal(Flow.edgeLabel("resume", "control", trace, es.control), "GetActor + ResumeActor (already RUNNING)");
  assert.equal(es.herd, "off");
  assert.equal(es.vm, "off");
  assert.notEqual(Flow.edgeLabel("resume", "vm", trace, es.vm), "restore");
  assert.equal(Flow.nodeStates("resume", trace).worker, "skipped");
});

test("a lifecycle call's vm edge needs the worker's line, not only the actor's state", () => {
  const nodes = (worker) => ({
    nodes: {
      browser: { state: "observed" },
      bridge: { state: "observed" },
      daemon: { state: "observed" },
      ateapi: { state: "observed" },
      worker: { state: worker },
      actor: { state: "observed" },
    },
  });
  for (const op of ["suspend", "resume", "fork"]) {
    assert.equal(Flow.edgeStates(op, nodes("observed")).vm, "done", op);
    assert.equal(Flow.edgeStates(op, nodes("missing")).vm, "missing", op);
  }
  assert.equal(Flow.edgeLabel("resume", "vm", nodes("observed"), "done"), "restore");
});
