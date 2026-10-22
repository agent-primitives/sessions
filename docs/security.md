# Security posture

What `agentsessions` protects, what it does not, and how to deploy it accordingly.

This is the operator's view. For reporting a vulnerability, see [`SECURITY.md`](../SECURITY.md).

## The short version

> `agentsessions` protects the **integrity of the record**. It does not protect **access to the
> system**.

Everything below follows from that split. The project makes strong, testable claims about what a
session log says and whether it can be trusted. It makes no claims at all about who is allowed to
reach the reference daemon, which does not check caller authority. Custom Go hosts can opt into
per-RPC checks, but complete required-mode deployment is not available (below).

## What is protected

These are enforced in code and exercised by the conformance suite.

**The log is tamper-evident.** Every record is hash-chained over a canonical form (RFC 8785 JCS over
proto3-JSON), so altering or reordering a record, or removing one that is not at the end, breaks
verification. Truncating the tail, or replacing the whole log, leaves a chain that verifies;
detecting it needs an external anchor such as a recorded head hash. The canonicalization is
language-neutral by design: `hack/verify_chain.py` is an independent Python implementation that
reproduces the Go hashes, so an auditor does not have to trust the Go binary that wrote the journal.
The host checks the chain itself before it uses a journal: `Exec`, reconstruction, recovery and
`Fork` refuse a journal that fails verification (`DATA_LOSS`) before running the harness, calling a
model or tool, or appending, and `Fork` before checkpointing the parent or provisioning a child, so
a record changed in storage is not served, extended or copied.

**A superseded writer cannot corrupt a session.** Each incarnation holds a fencing token, and the log
rejects an append carrying a stale one. A process that was presumed dead and then wakes up cannot
write. Combined with a compare-and-swap on append, that is what makes "single writer" enforceable
rather than aspirational.

**Replay does not re-execute side effects.** Reconstructing a session serves recorded model results
from the journal and never calls a provider, so replaying someone's session cannot spend their
budget or trigger a new external action.

**Payloads stay out of the logs.** Operational logging records structure — session ids, counts,
outcomes — and never message contents, event payloads, or fence values.

**Model credentials stay out of argv.** `agentsessionsd` reads the model key from the environment
and never accepts it as a flag, so it does not appear in the process list or shell history.

**A model key is not sent over plain `http`, except to loopback or a trusted service name.** With a key set, `agentsessionsd`
refuses to start when `-model-base-url` is `http` to a host other than a loopback address,
`localhost`, or a name ending in `.svc` or `.svc.cluster.local`. The service-name allowance is a
trusted-network exception, not a guarantee: it checks the name only, nothing proves the name
resolves to a cluster service, and the key and prompts cross the pod network unencrypted. Use
`https` unless you trust that network. With `HTTP_PROXY` set, Go sends a plain-`http` request to a
`.svc` name through that proxy, key and prompts included, and the proxy is usually outside the
network you trusted; loopback and `localhost` are never proxied. Add `.svc,.svc.cluster.local` to
`NO_PROXY` (a leading dot matches every name under it).

**Model credentials belong in a header, not the URL.** `agentsessionsd` logs the model endpoint at
startup, including the query of `-model-base-url` and of `-model-path`; only a userinfo password is
masked. Do not put a key, signature or token in either query: set the key in `MODEL_API_KEY` and
name its header with `-model-auth-header`.

## What is not protected

**The reference daemon has no authentication.** Its gRPC listeners use plaintext transport with
no credentials. Any process that can reach the port can call every RPC. Optional Go authentication
interceptors are available for custom hosts (below), but the daemon does not install them.

**The default Sessions service has no authorization.** Custom hosts must explicitly install
`WithAuthorization` (below); the daemon does not. `IdentityRef` is metadata, not a control.
Anyone who can reach the default service can discover a pending approval through Get/List and
approve or deny it. By default, `ApproveRequest.identity` is copied to the decision's `Event.actor`
as caller-supplied provenance;
`--actor`, issuer and subject flags are not credentials or permission checks. The durable gate
controls event ordering and effect recovery, not who is entitled to decide.

**`project` is not a tenancy boundary.** It is an exact-match filter on listing. A caller that names
another project gets that project's sessions. Do not treat it as isolation.

**There is no transport security.** This repo sets up no TLS anywhere, including between the host
and a harness. Traffic is readable and modifiable in flight by anything on the path. The one
exception is outside this repo: on substrate, the router-to-worker hop is mTLS (below), but the
host-to-router hop is not.

**On substrate, anything in the cluster can drive any session's harness.** The substrate backend
reaches each harness through substrate's `atenet-router`, over plaintext h2c, naming the actor in an
`ate-target-actor: <atespace>/<actor>` header. The router does not authenticate or authorize callers:
it resumes whichever actor the header names and proxies the call to it (substrate's threat model
tracks this as T-04, "check client permissions before resuming actors", not yet implemented), and
the NetworkPolicy a v0.5.0 install puts on it admits any source on its data ports (substrate
`manifests/ate-install/ate-system-networkpolicies.yaml`). So any pod that can reach
`atenet-router.ate-system.svc:80` can wake any actor and open a `Harness.Connect` stream to it, and
the harness accepts the stream, because it does not authenticate the host either. Such a caller can
feed a harness forged model results or tool results, read what the harness emits, and keep an actor
from suspending: substrate drains open streams before it snapshots, and `Placer.Suspend` only
refuses to overlap operations of its own Registry, so it neither sees nor ends a stream another
caller holds open. The hash chain stays intact, because only the real host writes the journal, but
anything the harness emits afterwards, including what the real host then records, can reflect the
forged input. For a `REQUIRES_MEMORY_SNAPSHOT` harness the forged input lands in RAM and persists
through suspend, resume and fork, so a verified chain is not evidence that its content is
unpoisoned. Only the router-to-worker hop is protected
(mTLS with the router's SPIFFE identity, and the worker checks the actor is assigned to it).

Until this is closed, run the substrate backend only in a cluster where every workload that can
reach the router is trusted, or restrict the router to the `agentsessions` host. NetworkPolicies are
additive, so a narrower policy next to the install's `atenet-router` policy restricts nothing: the
install's policy itself has to change, and every Substrate install applies it again.

The `ate.dev/api-access=true` label that v0.5.0 requires on the `agentsessionsd` namespace
([namespace access](substrate-conformance.md#substrate-v050-namespace-access-to-ate-api-server))
admits every pod in that namespace except worker pods to ate-api-server at the network level.
ate-api-server still authenticates and authorizes each request (as the policy's comment in substrate
`manifests/ate-install/ate-system-networkpolicies.yaml` notes), so the label widens reachability,
not permissions. Keep untrusted workloads out of a labeled namespace.

Not yet implemented: the harness does not authenticate the host. A planned fix delivers a per-session
credential at actor creation and has `harnessnode` check it on every Connect, so a harness talks only
to its own session's host whatever the ingress allows. Router-side caller authorization (T-04)
belongs upstream in substrate and would complement it, not replace it.

**The harness is trusted code.** The host mediates model calls and host-executed tools, which is what
makes replay exact, but that is a determinism mechanism, not a containment one. Whatever isolation a
harness has comes from the `Runtime` backend underneath it, and the backends differ sharply:
`runtime/local` serves the harness **inside the host process** over a unix socket, so a harness there
shares the host's memory, filesystem, and credentials. A sandboxed backend is what puts a boundary
there; the local one has none.

**A harness registered by address is whatever answers there.** `runtime/remote` (`agentsessionsd
--harness name=address`) dials the operator-configured address over cleartext h2c with no
authentication of either side. Sessions clients cannot supply an address; operators can configure
one at startup or register one through the separate operator listener when enabled.
Anything that can listen on that address, or sit on the network path to it, is the harness: it
writes every event the session journals, chooses which model calls the host makes and pays for with
its credential, and sees the full history the host sends each turn. The chain makes later tampering
with the journal detectable; it cannot tell a forged harness from the real one. The optional operator
registry can also register a remote address at runtime; unlike the startup `--harness` flag, this
requires access to the separate operator port. Bind the harness to
loopback or a unix socket on the host, or keep the hop on a network you trust entirely. For a unix
socket, "can listen on that address" means "can write to the socket's directory", so put it in a
directory only the harness's user can write, not a shared one such as `/tmp`. `cmd/harnessnode`
refuses a lock file next to its socket that is a symlink, a hard link, or owned by another user, so
a hostile directory cannot redirect its writes, but it cannot stop another user who can write there
from binding the socket first. The host
does refuse a harness that declares `REQUIRES_MEMORY_SNAPSHOT`, but that is a correctness check on
what the harness declares, not authentication.

That check also has a known limit. The host checks a harness's capabilities with a `Describe` call
before it opens the turn's `Connect` stream, and the answer is not bound to that stream. If a
different harness takes over the same address between the two calls, for instance because the
harness restarted, gRPC reconnected to another replica, or a proxy routed the two calls to different
backends, the turn runs on that harness and the host does not detect it. Registering a harness by
address assumes the operator controls what serves that address, and that every harness that can
answer there declares the same resumability.

**Registering a harness is an admin action.** A caller that can register a harness chooses what the
host dials, which means where it sends every session's history and whose model credential pays for
that harness's calls. The `HarnessRegistry` API therefore has no place on the Sessions port, which
anyone who can reach it can call. `agentsessionsd` only serves it when `--registry-addr` is set to
a **literal loopback IP:port** (for example `127.0.0.1:8081`); wildcard, hostnames including
`localhost`, and non-loopback IPs are refused. The listener is separate from Sessions, off by
default, plaintext and **unauthenticated**. Sessions remains plaintext and unauthenticated too; it
never serves registry RPCs. Loopback limits network reachability, **not identity**: any local process,
including another container in the same Kubernetes pod (which shares its network namespace), can
administer registrations. It is not per-user isolation. An embedder that serves the API must keep
it off the Sessions listener and control access to it. A registered remote harness
is whatever answers at its address, as above. `HarnessSpec.descriptor_id` makes the host refuse a
turn when the harness there reports another descriptor id, which catches a misdirected address; the
id is what the harness says about itself, so it does not authenticate the harness.

## Deletion is not erasure or containment

Any caller that can reach the default Sessions service can permanently tombstone a known UID:
Delete has no additional authorization unless the host installs `WithAuthorization`. `PENDING`
acknowledges durable intent only until #88 provides authoritative ownership and cleanup. Actors,
snapshots and fork-child pins remain retained indefinitely; Delete does not
call Stop, reclaim resources or stop an actor's independent external activity. The retained metadata
and journal are audit evidence, not a complete inventory of resources to clean up.

UID, metadata and journal remain forever and are available through Get and raw Replay even when
List hides them. Deletion is neither data erasure nor access revocation; it does not cascade to
children. Do not store secrets expecting deletion to remove them. Exec, historical Fork, Resume,
Suspend and Approve (including exact retries) are refused for the tombstone, but the
same-Registry admission guard does not fence another host's compute provisioning before its
first durable fence. Keep runtime-side reconciliation separate and scoped to provable ownership.
See [deletion semantics](concepts.md#permanent-soft-deletion).

## Approval is not a security policy

`REQUIRES_APPROVAL` is supported only for stateless replay. The RPC/SDK commits a decision without
compute or an executor call; explicit Resume performs recovery. CLI approve/deny do both, and a
recovery failure leaves the decision committed. The default Sessions service has no caller
enforcement; custom hosts can require an independent approve permission (below). There is no
tool/resource policy evaluator, approval timeout, expiry, automatic decision or implemented Cancel
path. Unresolved requests and suspended snapshots/handles are retained indefinitely; operators must
manage retained resources outside these APIs.

A custom host executor must authorize tools/resources itself and durably deduplicate by **session UID
plus idempotency key**, storing the original receipt across host restarts. Resume re-drives an
interrupted ordinary call only for tools declared with `placement.WithDurableDedup`; a false
declaration is an effect that can repeat. A hash-chained request or
decision alone does not make delivery exactly once. Two sessions using the same harness-chosen key
are distinct namespaces. The stock daemon can optionally configure
[Unix-socket executors](tool-executors.md) for CLI `-harness` entries, dispatching only tools that
declare safe durable dedup. Keep sockets in private directories with restricted permissions, and
credentials in the executor, not config. Read-only or naturally idempotent tools may declare true
when repeating them is harmless. An approved gate without an executor stays recoverable and fails
Resume with `FAILED_PRECONDITION`; denial requires no executor and records a denied receipt.
Configured executor errors instead become durable `EXECUTOR_ERROR` receipts: the effect may have
committed before the response was lost, and recovery does not retry a receipted failure. Neither path
prevents a trusted harness from doing unmediated work outside the sink.

## Operator access

For a local host with a remote harness, run the daemon with a real model and listen only on
loopback (the operator port is optional; Sessions still uses its existing `-addr` setting):

```sh
MODEL_API_KEY=... agentsessionsd -addr 127.0.0.1:8080 -registry-addr 127.0.0.1:8081 \
  -journal agentsessions.db -model your-model -model-base-url https://trusted-model.example/v1
agentctl harness register --server 127.0.0.1:8081 --name mine --spec spec.json
agentctl harness get --server 127.0.0.1:8081 --name mine --observe
agentctl harness list --server 127.0.0.1:8081 --include-retired --page-size 50
agentctl harness retire --server 127.0.0.1:8081 --name mine --reason maintenance
```

`spec.json` is a strict proto-JSON `HarnessSpec`, for example
`{"remote":{"address":"127.0.0.1:9000"},"capabilities":{"resumability":"RESUMABILITY_STATELESS_REPLAY"},"descriptor_id":"echo"}`.
Both new and stored remote registrations need `-model` (and a model key when that endpoint
requires one), so model calls cannot silently use the built-in echo stub. Without `-model`, stored
remote registrations load as unservable and their existing sessions fail `FAILED_PRECONDITION`,
whether active or retired, even with the operator listener off. Static inspection, listing and
retirement do not require `-model`. This reference daemon does not serve substrate placements. Registry
responses print proto-JSON, including `spec_digest` and lifecycle state.

The checked-in [Kubernetes manifest](../deploy/kubernetes/agentsessionsd.yaml) pins v0.2.0, which
supports `-registry-addr` but leaves it off: add `-registry-addr 127.0.0.1:8081` to its args first.
The manifest does not expose the operator port through a Service. With the listener enabled, an
operator with **pods/portforward RBAC** may open a local tunnel, then use the CLI against its local
endpoint:

```sh
kubectl -n agentsessions port-forward --address 127.0.0.1 deployment/agentsessionsd 18081:8081
# In another terminal:
agentctl harness list --server 127.0.0.1:18081 --include-retired
```

Explicitly bind kubectl's local endpoint to `127.0.0.1` with `--address`, not `0.0.0.0`. Grant
`pods/portforward` and `pods/exec` narrowly: exec access also reaches the pod's loopback. Port-forward
RBAC controls who can open a tunnel, **not** authorization for processes already inside the pod.
Do not expose the registry through a Kubernetes Service or Ingress. The daemon's operator port has
no independent token or TLS. [#127](https://github.com/agent-primitives/sessions/issues/127) tracks
covering it with the same verifier and authorizer as Sessions, using a separate operator role.
Until then it stays loopback-only and off by default. Anyone who can reach this port can change
destinations for session history and spend the host's model budget.

## Deploying it

Given the above, there is one safe shape for this release:

- Run it on **loopback**, or on a network segment you already trust entirely.
- Treat every host as **single-tenant**. One project per host if you need separation.
- Do not put it behind a public ingress, even an authenticated one, unless that proxy is doing
  **all** of the authentication and authorization and nothing else can reach the port.
- Keep the journal file's permissions tight. It holds full conversation contents and opaque execution
  config in the clear, and anyone who can read it can read every session. Do not put credentials in
  execution config; operational logging omits it, but the durable journal and Replay API expose it.
- Anyone who can **write** the journal can rewrite history. The chain makes that detectable, not
  impossible: verification tells you the log was altered, it does not prevent the alteration. The
  chain is also unkeyed, so a writer that recomputes every hash after an edit, or removes records
  from the end, passes verification, including the host's own check before it uses a journal.
  Journal write access is also as powerful as registry access: the daemon loads stored remote
  addresses even with the operator listener off and dials them when observing or placing sessions,
  so a writer can choose where session history goes and whose calls spend the host's model budget.

## Host-side ownership and authorization contracts

`api.CallerIdentity` is an exact verified caller issuer/subject pair, separate from agent
`IdentityRef` provenance. Trusted Go hosts can atomically create an owned session through
`sqlitelog.Store.CreateOwnedSession`, including active-harness admission. Existing UIDs cannot be
claimed, even when unbound or deleted. Metadata updates, fencing and appended/copied history do
not assign or replace owners. Legacy sessions and sessions created by unenforced RPCs remain
unbound; ownership is not inferred from provenance. Unbound sessions are denied by
`OwnerAuthorizer`, not treated as public. Owner metadata is not hash-chain attested; database
writers are trusted.

The host-side `api.Authorizer` contract has an owner-only implementation. `OwnerAuthorizer` grants
only ordinary session actions to an exact owner; create, approval decisions, audience minting and
unknown actions are denied. It does not verify credentials. `ActionFork` authorizes the parent
only, not child ownership. The current fork path creates unbound child rows before metadata
publication; `CreateOwnedSession` cannot claim them afterward. Enforcing hosts must not expose
owned forks until a separate primitive reserves verified child ownership before fencing or copying
logs, with publication and failure cleanup defined. This must preserve fork admission for existing
sessions on retired harnesses; ordinary create admission is not a substitute. This is a prerequisite
for enforcement ([#127](https://github.com/agent-primitives/sessions/issues/127)).

`ListOptions.Owner` filters an exact pair in SQL before paging; enforcing callers must always set
it, since nil retains the full project listing. Its unsigned cursors are not caller-bound credentials.

The daemon does not install these contracts. Custom hosts can opt into the RPC checks below,
but that is not a deployable required mode. SQLite schema v5 carries an off-mode authorization
marker; this host refuses required, unknown or missing markers even at the same schema version.
There is no supported switch to enable required mode. The marker is checked only at Open.
Changing it requires stopping every writer, including embedded CLI processes, then restarting all
hosts with binaries that support the new mode; already-open hosts do not recheck it.
Owner identifiers are retained metadata: deletion is not erasure. Do not expose a host on an
untrusted network.

## Optional Go RPC authorization

Custom hosts can pass `session.WithAuthorization(policy)` to `NewService` and
`session.WithRegistryAuthorization(policy)` to `NewHarnessRegistry`. Each option requires a verified
`authn.Caller(ctx)` even for direct handler calls: missing caller returns `UNAUTHENTICATED` before
request validation. A nil policy fails closed with `INTERNAL`, not back to off. Constructors without
these options retain trusted-network behavior, even if authentication interceptors are installed.

Checks precede runtime IO, fences, stream frames and approval repair. Each Go
`api.AuthorizationRequest.Method` carries the full originating RPC path, for example
`/agentsessions.v1.Sessions/Exec` or `/agentsessions.v1.HarnessRegistry/GetHarness`, including
for direct handler calls. Compound, recovery and global checks retain that originating path:
inputless Exec approval recovery checks `resume` with the Exec method, not the Resume method.
`Method` is a Go SPI field, not a protobuf message or wire field.

| RPC | Required actions |
| --- | --- |
| GetSession, Replay | `read` |
| Exec | `exec`; also `resume` when inputless Exec enters approval-recovery Resume |
| CreateSession | global `create` |
| Exec with no session | global `create`, then `exec` on the prospective owned UID, before insert |
| Suspend | `suspend` |
| Resume | `resume` and `exec` |
| DeleteSession | `delete` |
| Approve | `read` and independently `approve` |
| Cancel | `cancel`, then `UNIMPLEMENTED` |
| Fork | `fork` on parent and global `create`, then `FAILED_PRECONDITION` (owned publication unavailable) |
| ListSessions | global `list`, then exact verified-owner SQL filtering before keyset pagination |
| GetHarness (including observe), ListHarnesses, RegisterHarness, RetireHarness | global `registry.read`, `registry.list`, `registry.register`, `registry.retire`, respectively |

Ordinary Exec can restore suspended compute with `exec` alone; only inputless approval recovery
adds `resume`. The Resume RPC always requires both `resume` and `exec`, even for a no-op.

Global actions use empty UID/owner. Create and auto-create insert the verified caller as immutable
owner; they never claim existing rows. The service passes stored owners, not request provenance,
to the policy. Existing unbound rows reach custom policies with a zero `Owner`; `OwnerAuthorizer`
denies them, but a custom policy can allow them. Unknown UIDs and policy refusals return the same
generic `PERMISSION_DENIED` once the deletion probe permits lookup. Only
`errors.Is(err, api.ErrPermissionDenied)` is a policy denial; unexpected policy/store failures and
enforced RPC `INTERNAL` diagnostics are sanitized. Caller cancellation and deadlines retain their
codes. Policies own their failure logs and metrics; the service never logs raw policy causes.
In enforced mode only, `INTERNAL` diagnostics include `request_id`, `method`, `error_kind` and a
stable `error_category`. Known trusted host/store errors log the full cause with category `host`.
Model/provider/harness and other unknown-origin errors never log message text: category is
`execution` for Exec/Resume, `runtime` for Suspend/Fork, or `external` for other methods. Unknown
mixed execution/runtime phases fail closed; a store fault deep inside runtime is not trusted unless
known at the service storage boundary. There is no heuristic redaction. Request metadata,
credentials and session payloads remain excluded; trusted/off behavior is unchanged. This is a
diagnostic-only exception to the [operational log data policy](observability.md#data-policy), not
permission to put sensitive data in errors. Custom policies own any additional sharing rules;
listing remains owner-only, not a list of arbitrary grants.

Mutation owner lookups for Exec, Suspend, Resume, Fork, Approve and DeleteSession first probe
shared deletion admission after authentication. The probe acquires and immediately releases
admission before owner SQL or policy evaluation; it never leases SQL. An exclusive Delete already
in progress at the probe returns `ABORTED` before lookup or policy, even for a verified non-owner
who knows the UID. This narrow disclosure grants no access. The tiny shared probe can also race
an owner's Delete, briefly returning `ABORTED` to Delete; this is not an owner-query SQL hold. The
probe can race a later Delete; the mutation's later guards remain authoritative. An owner query,
including one later denied by policy, holds no admission while waiting for SQL and cannot keep
Delete out that way. Outside probe contention, inaccessible sessions return `PERMISSION_DENIED`.
GetSession and Replay remain unguarded audit reads; Cancel is unchanged.

`api.OwnerAllowlistAuthorizer` delegates ordinary session actions to `OwnerAuthorizer` and adds
explicit exact issuer/subject allowlists via `Allow map[api.Action][]api.CallerIdentity` for create,
list, approve and registry actions. Unknown actions and mint stay denied even if allowlisted.
An owner alone cannot approve: this policy requires an owner who is also on the approve allowlist.
A separate non-owner approver needs a custom read policy. Configure policies before serving; do not
mutate their allowlists concurrently.

Enforced Approve ignores request identity and clears the actor before decision/retry comparison.
It does **not** yet journal a verified caller or distinguish required-mode decisions from anonymous
off-mode history. Historical decisions with a nonempty actor are not identical retries after this
normalization. Verified attribution and caller-bound retries require a versioned journal contract.

**This is not cross-host isolation or supported required-mode activation.** Journals remain marked
`off`, so unenforced hosts and CLI processes can still read/write owned sessions, create unbound
rows, and bypass these checks. Do not share an enforcement test journal with them. Owned Fork
remains refused before discovery or effects until ownership can be reserved before the first copied
record. Complete activation also needs verified approval audit, an immutable journal policy marker
and startup validation, a daemon required switch, and TLS assembly with server identity validation
([#127](https://github.com/agent-primitives/sessions/issues/127)). Registry stays on its separate,
opt-in loopback listener; there is no same-pod/self-dial exemption or implicit credential
forwarding.

## Optional Go caller authentication

Custom hosts can install both interceptors returned by `authn.NewInterceptors` on a gRPC server.
Install them before any interceptor that discloses data or performs effects. A panic-recovery
interceptor may wrap them; grpc-go does not recover verifier panics.
`authn.Required` requires a `CallerVerifier`; `authn.Off` skips verification and installs no caller.
The verifier reads incoming metadata or verified TLS peer information and returns only an exact
issuer/subject pair and a future credential expiry, never credentials. Adapters must reject missing,
malformed, duplicate and expired credentials, validate signatures, exact allowed issuers,
per-issuer audiences and explicit claim mappings. Do not discover trust from untrusted issuer URLs.
Adapters also own failure logging and metrics, without credential material. The interceptors
sanitize verifier errors on the wire and do not log or count them. No credential format or
production verifier is bundled.

The interceptors validate identity/expiry and return generic `UNAUTHENTICATED` on verification
failure, without anonymous fallback or method exemptions. Health and reflection on the same server
also require credentials; kubelet gRPC probes cannot send them. Hosts should serve health and
reflection on a separate server/listener, with appropriate network access restrictions.
`authn.Caller(ctx)` exposes the verified pair; request identity and forwarded headers do not establish
it. Unary calls authenticate once at admission: credential expiry does not add a handler deadline,
end the call, or remove its verified caller. The caller's own cancellation/deadline still applies,
but the interceptor preserves the handler's returned result and status.
Only streams get the earlier caller deadline or verified expiry; expiry reports `UNAUTHENTICATED`,
while caller cancellation/deadlines retain their codes. Stream expiry overrides even a successful
handler return; effects may already have completed. Handlers must cooperate with cancellation. There
is no in-stream refresh, interruption of transport I/O already in progress, or rollback of an
in-flight effect. If expiry interrupts a running `Exec` harness and it returns an error, the controller
journals a best-effort `ERROR`, unless stopped after an approval intent. Retrying `Exec` with inputs
and a fresh credential starts a new turn, rather than automatically continuing the interrupted one;
pending approvals still follow the recovery rules in spec §11.4 (`OPS-14`). An `ERROR` does not prevent
later `Resume` recovery. Expiry before the harness runs need not journal an `ERROR`.
Verification runs once per RPC: revocation before `ValidUntil` is not noticed by an admitted unary
call or an open stream.

Authentication alone is not isolation. Hosts still need authorization before disclosure/effects,
TLS with server identity validation, and a compatible journal policy. None is assembled by these
interceptors; the reference daemon has no required-mode switch. This does not secure the harness,
router or model endpoints.
