// flow.js: the request-flow topology and its pure rules, shared by the page (app.js) and its tests
// (flow.test.js, run with `node --test`). No DOM here.
//
// The diagram is the path a lifecycle call takes from this page to the session's actor, as the
// example deploys it (demo/run.mmd and demo/diagrams.md cite the code for each arrow). Which hops a
// call lights comes from the bridge's trace: a component's own log line, or the actor's state read
// from ate-api. A hop with no evidence stays unlit; it is never assumed. The control and atelet
// edges are labelled with the method the trace matched, not the method the call usually makes.
(function (root, factory) {
  const flow = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = flow;
  } else {
    root.Flow = flow;
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  const NODE_W = 150;
  const NODE_H = 54;
  const VIEW = { width: 1200, height: 238 };

  const nodes = [
    { id: "browser", label: "Browser", sub: "this page", x: 0, y: 24 },
    { id: "bridge", label: "lifecycle-ui", sub: "loopback bridge", x: 210, y: 24 },
    { id: "daemon", label: "agentsessionsd", sub: "journal + substrate adapter", x: 420, y: 24 },
    { id: "ateapi", label: "ate-api", sub: "substrate control plane", x: 630, y: 24 },
    { id: "worker", label: "worker pod", sub: "atelet + ateom-microvm", x: 840, y: 24 },
    { id: "actor", label: "actor", sub: "harness in a micro-VM", x: 1050, y: 24 },
    { id: "model", label: "model", sub: "chat completions", x: 420, y: 162 },
    { id: "router", label: "atenet-router", sub: "turn traffic", x: 630, y: 162 },
  ];

  const edges = [
    { id: "http", from: "browser", to: "bridge", label: "HTTP" },
    { id: "grpc", from: "bridge", to: "daemon", label: "gRPC" },
    { id: "control", from: "daemon", to: "ateapi", label: "control" },
    { id: "herd", from: "ateapi", to: "worker", label: "atelet" },
    { id: "vm", from: "worker", to: "actor", label: ":80" },
    { id: "model", from: "daemon", to: "model", label: "" },
    { id: "wire", from: "daemon", to: "router", label: "harnesswire" },
    { id: "route", from: "router", to: "worker", label: ":443" },
    { id: "replay", from: "bridge", to: "router", label: "replay: controller.Replay through a port-forward" },
  ];

  // The edges each call may take, in request order. Exec and Resume skip the restore when the actor
  // is already running (exec only reads it with GetActor; Resume gets resumed=false back); the
  // trace shows which ones the call took.
  const paths = {
    create: ["http", "grpc"],
    exec: ["http", "grpc", "control", "herd", "vm", "wire", "route", "model"],
    suspend: ["http", "grpc", "control", "herd", "vm"],
    resume: ["http", "grpc", "control", "herd", "vm"],
    fork: ["http", "grpc", "control", "herd", "vm"],
    replay: ["http", "grpc", "replay", "route", "vm"],
  };

  // What each edge means for a call, shown on a lit edge. The control and atelet edges are not
  // here: their label is the method the trace found (methodFrom).
  const methods = {
    create: { grpc: "CreateSession" },
    exec: { grpc: "Exec (stream)", vm: "forward :80", wire: "turn stream", model: "model call" },
    suspend: { grpc: "Suspend", vm: "checkpoint" },
    resume: { grpc: "Resume", vm: "restore" },
    fork: { grpc: "Fork", vm: "restore" },
    replay: { grpc: "Replay (journal)", replay: "controller.Replay", vm: "re-run turns" },
  };

  // The node whose trace evidence carries an edge's method.
  const methodFrom = { control: "ateapi", herd: "worker" };

  // The node whose evidence shows an edge was taken, where that is not the edge's target. A turn
  // reaches the actor through atenet-router and atunnel in the worker pod, which logs nothing per
  // request, so the router's line naming the session is the evidence for both hops. A lifecycle
  // call's restore or checkpoint is the worker's RestoreWorkload or CheckpointWorkload line: the
  // actor's state read shows the actor, not that this call changed it (a Resume of a RUNNING
  // actor restores nothing).
  const witness = {
    exec: { route: "router", vm: "router" },
    replay: { route: "router", vm: "router" },
    suspend: { vm: "worker" },
    resume: { vm: "worker" },
    fork: { vm: "worker" },
  };

  const nodeByID = new Map(nodes.map((n) => [n.id, n]));

  function box(id) {
    const n = nodeByID.get(id);
    return { x: n.x, y: n.y, w: NODE_W, h: NODE_H, cx: n.x + NODE_W / 2, cy: n.y + NODE_H / 2 };
  }

  // edgePath returns the SVG path of an edge: straight along the top row, elbows to the bottom row.
  // The routes are fixed so no two edges cross.
  function edgePath(id) {
    const e = edges.find((x) => x.id === id);
    if (!e) return "";
    const a = box(e.from);
    const b = box(e.to);
    switch (id) {
      case "model":
        return `M${a.cx} ${a.y + a.h} V${b.y}`;
      case "wire":
        return `M${a.x + a.w} ${a.y + a.h - 10} H${a.x + a.w + 30} V${b.cy} H${b.x}`;
      case "route":
        return `M${a.x + a.w} ${a.cy} H${b.cx} V${b.y + b.h}`;
      case "replay":
        return `M${a.cx} ${a.y + a.h} V${VIEW.height - 8} H${b.cx} V${b.y + b.h}`;
      default:
        return `M${a.x + a.w} ${a.cy - 7} H${b.x}`;
    }
  }

  // labelAt is where an edge's label sits.
  function labelAt(id) {
    const a = box(edges.find((x) => x.id === id).from);
    const b = box(edges.find((x) => x.id === id).to);
    switch (id) {
      case "model":
        return { x: a.cx + 8, y: (a.y + a.h + b.y) / 2 + 4, anchor: "start" };
      case "wire":
        return { x: a.x + a.w + 38, y: b.cy - 30, anchor: "start" };
      case "route":
        return { x: a.x + a.w + 10, y: a.cy - 8, anchor: "start" };
      case "replay":
        return { x: a.cx + 10, y: VIEW.height - 14, anchor: "start" };
      default:
        // Above the row, centered on the gap: a method name is wider than the gap between nodes.
        return { x: (a.x + a.w + b.x) / 2, y: a.y - 8, anchor: "middle" };
    }
  }

  function pathFor(op) {
    return paths[op] ? paths[op].slice() : [];
  }

  // nodesOn lists the nodes a call's path touches, in the order the request reaches them.
  function nodesOn(op) {
    const seen = [];
    for (const id of pathFor(op)) {
      const e = edges.find((x) => x.id === id);
      for (const n of [e.from, e.to]) if (!seen.includes(n)) seen.push(n);
    }
    return seen;
  }

  // traceStates merges a trace into a state per node: "off" when not on the call's path; on it, the
  // trace's state (observed, missing, refused, failed, skipped, unknown), or "pending" with no
  // trace yet.
  function traceStates(op, trace) {
    const on = new Set(nodesOn(op));
    const out = {};
    for (const n of nodes) {
      if (n.id === "model" && op === "replay") {
        out[n.id] = (trace && trace.nodes && trace.nodes.model && trace.nodes.model.state) || "pending";
        continue;
      }
      if (!on.has(n.id)) {
        out[n.id] = "off";
        continue;
      }
      const ev = trace && trace.nodes ? trace.nodes[n.id] : null;
      out[n.id] = ev ? ev.state : "pending";
    }
    return out;
  }

  // edgeStates: an edge on the path is "done" when its witness (its target, unless witness says
  // otherwise) was observed (or refused, declined or failed: the request reached it), "missing"
  // when the witness has no evidence, "pending" with no trace, and "off" when not on the path or
  // not taken (skipped).
  function edgeStates(op, trace) {
    const ns = traceStates(op, trace);
    const on = new Set(pathFor(op));
    const out = {};
    for (const e of edges) {
      if (!on.has(e.id)) {
        out[e.id] = "off";
        continue;
      }
      const s = ns[(witness[op] && witness[op][e.id]) || e.to];
      if (s === "observed" || s === "refused" || s === "declined" || s === "failed") out[e.id] = "done";
      else if (s === "pending") out[e.id] = "pending";
      else if (s === "skipped") out[e.id] = "off";
      else out[e.id] = "missing";
    }
    return out;
  }

  // nodeStates is what each node shows: its trace state, except that a skipped node a lit edge
  // passes into is "forwarded": the call went through it, and it logs nothing for that.
  function nodeStates(op, trace) {
    const out = traceStates(op, trace);
    const es = edgeStates(op, trace);
    for (const e of edges) {
      if (es[e.id] === "done" && out[e.to] === "skipped") out[e.to] = "forwarded";
    }
    return out;
  }

  // edgeLabel is the text on an edge: the method the trace found for the control and atelet
  // edges, else what the edge means for this call, else its plain name.
  function edgeLabel(op, id, trace, state) {
    const base = edges.find((e) => e.id === id).label;
    if (state === "off") return base;
    if (methodFrom[id]) {
      const ev = trace && trace.nodes ? trace.nodes[methodFrom[id]] : null;
      return (ev && ev.method) || base;
    }
    return (methods[op] && methods[op][id]) || base;
  }

  // refusalRows turns a parsed refusal into labelled rows for the card, in reading order. Missing
  // fields are left out rather than shown blank.
  function refusalRows(r) {
    if (!r) return [];
    const rows = [];
    if (r.session) rows.push(["Session", r.session]);
    if (r.actor) rows.push(["Actor", r.actor]);
    if (r.required) rows.push(["Required fidelity", r.required]);
    if (r.observed) rows.push(["Observed fidelity", r.observed]);
    return rows;
  }

  // failedNode is the node a failed call stopped at: the bridge when it says so (a replay after
  // its journal read), else the daemon, which returned the error.
  function failedNode(act) {
    return act && act.failedAt === "bridge" ? "bridge" : "daemon";
  }

  // replayNote is the timeline's line for a replay that got past the journal read. Only a replay
  // that held claims re-execution and zero model calls; one that stopped says what it established.
  function replayNote(act) {
    const r = act && act.replay;
    if (!r) return null;
    const turns = r.turns + " completed turn" + (r.turns === 1 ? "" : "s");
    let fake = "";
    if (r.fakeBefore >= 0 && r.fakeAfter >= 0) {
      fake = "; fake model counter " + r.fakeBefore + " to " + r.fakeAfter;
    } else if (r.fakeBefore >= 0) {
      fake = "; fake model counter " + r.fakeBefore + " before, not read after";
    }
    if (act.ok) {
      return "Re-ran " + turns + " in the actor; controller counted " + r.controllerCalls +
        " model calls" + fake + ". Chain verified, head " + r.head + ".";
    }
    const ctl = r.controllerCalls >= 0
      ? "the controller counted " + r.controllerCalls + " model calls before it stopped"
      : "the controller did not re-run any turn";
    const verdict = r.modelCalled ? "The replay called the model." : "Zero model calls is not established.";
    return "Replay did not complete. The journal (" + r.records + " records, " + turns + ", head " +
      r.head + ") verified before the run; " + ctl + fake + ". " + verdict;
  }

  // replayBlock is why the Replay button is off for a session, or "" when it may run. The bridge
  // declines the same cases; this keeps the button from offering them. Replay drives the session's
  // running actor through atenet-router, which resumes a suspended actor on any routed request, so
  // only a session Sessions reports COMPUTE_LIVE is offered.
  function replayBlock(session, lane, replayOn) {
    if (!session || !lane) return "Select a session";
    if (!replayOn) return "Replay is off: start the page with -router";
    if (lane.tier !== "STATELESS_REPLAY") {
      return "Replay re-runs turns on the live actor; the memory tier keeps its conversation in RAM";
    }
    if (session.computeState === "COMPUTE_LIVE") return "";
    if (session.computeState === "COMPUTE_COLD") {
      return "Suspended: Resume first. Replay drives the running actor, and routing its turns would wake the actor outside Sessions";
    }
    return "Compute is " + String(session.computeState || "unknown").replace("COMPUTE_", "").toLowerCase() +
      ": run a turn first. Replay drives the running actor";
  }

  // outcome is a call's result as the page labels it: OK, Declined when the bridge refused it
  // before any lifecycle call (its gRPC code would read Unknown), else the call's gRPC code.
  function outcome(act) {
    if (!act) return "";
    if (act.ok) return "OK";
    if (act.declined) return "Declined";
    return act.code || "failed";
  }

  // concerns says whether an activity belongs to a session's view: its own call, or the fork that
  // made it. A call's result is drawn only beside the session it concerns.
  function concerns(act, uid) {
    return Boolean(act && uid) && (act.session === uid || act.child === uid);
  }

  // flowFor is the activity whose flow a session's view draws: the one asked for (a URL's activity
  // ID) when it concerns the session, else the session's latest call, else none.
  function flowFor(acts, uid, wantID) {
    const mine = (acts || []).filter((a) => concerns(a, uid));
    return mine.find((a) => a.id === wantID) || mine[mine.length - 1] || null;
  }

  return {
    VIEW,
    NODE_W,
    NODE_H,
    nodes,
    edges,
    methods,
    edgePath,
    labelAt,
    pathFor,
    nodesOn,
    nodeStates,
    edgeStates,
    edgeLabel,
    refusalRows,
    failedNode,
    replayNote,
    replayBlock,
    concerns,
    flowFor,
    outcome,
  };
});
