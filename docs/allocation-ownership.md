# Durable allocation ownership

Placement records runtime allocation intent in SQLite before I/O. A runtime with an identity-bearing
cleanup contract supplies a complete, immutable plan: one target for every dedicated resource that
operation may create. Create and Restore use the normal session recorder. Fork uses an unpublished
allocation recorder so the resource plan is durable without inserting a visible `sessions` row before
the child's existing first-fence publication boundary.

## Legacy handles and owned resources

A backend without `api.ResourceCleanupRuntime`, or one with an empty cleanup key, keeps the legacy
session-handle path. Placement records the operation before I/O and confirms the returned
`Incarnation.ID` on success. The ID is not immutable compute identity and cannot authorize name-only
deletion. Existing local and remote `api.CleanupRuntime` behavior is unchanged.

An identity-bearing backend implements `api.ResourceCleanupRuntime`. Its cleanup key identifies the
actual control-plane configuration, never a harness alias. `CleanupResources` returns UID-empty,
provider-opaque references before I/O. Core `api` knows neither actor/tag vocabulary nor namespace
layout. A Substrate backend encodes those details privately. If a configured owned backend cannot
record an unpublished Fork allocation, placement refuses before runtime I/O rather than silently
falling back to an untracked resource effect.

`Incarnation.Cleanup` carries immutable ownership evidence. `CleanupEvidence.Resources` returns a
defensive copy. A producer may return evidence with an error after a control-plane effect; placement
confirms that evidence on the separate bounded confirmation context and preserves both the runtime
and persistence error. An empty or lost response leaves targets unconfirmed. No code looks up the
current object by name and adopts its UID.

## SQLite v10 resource graph

Schema v10 keeps `allocations` for operation-level intent and adds one `allocation_resources` claim
per planned target. Each claim records its opaque reference, observed immutable UID, confirmation and
release state. `allocation_borrowers` names release dependencies. For a memory-tier Substrate fork,
the child actor borrows the tag's snapshot copy, so the tag remains blocked until the actor is
released.

A complete plan rejects missing references, duplicate references, self-borrowing and cycles before
I/O. Partial evidence may omit the borrower resource, which permits a confirmed tag to survive a
lost child-create reply while the actor target remains unresolved. SQLite checks every observed
reference and borrower against the complete durable plan. A result with an error is therefore useful,
but never permission to add a new target after the fact.

Exact duplicate claims keep their per-attempt rows. Compaction sets `canonical_resource_id` instead
of deleting the old retry receipt, so an identical late confirmation remains a harmless no-op.
Different UIDs remain separate. Queries and future release code resolve borrower endpoints through
the canonical claim. Alias creation rechecks the canonical graph for cycles before commit.

## Substrate producer and UID preconditions

Substrate's adapter now returns actor and tag UIDs from create calls. The backend keeps provider
reference parsing and UID-conditional deletion in `runtime/substrate`; the core API contains no
`actor`/`tag` enum and no Substrate `Namespace` field.

Create records an actor identity after the adapter proves this call created it, including when a
later egress-policy, resume or checkpoint-validation step fails. Restore returns ownership only when
a recorded actor UID matches the object it resumes. Merely observing a replacement is not ownership.
Fork records the tag and child actor separately. If `CreateActorFromTag` has an ambiguous result and
no child UID, the backend retains the exact tag: an unknown-UID child may still borrow it. It may
release a tag immediately only after a conclusive no-child result or exact known-child deletion.

All changed Substrate teardown calls require one nonempty immutable UID. `DeleteActor` runs directly
for RUNNING or suspended actors; no suspend step creates another snapshot. A NOT_FOUND result makes
an exact UID retry idempotent. Actor deletion precedes release of its incoming fork tag. A UID
mismatch leaves the replacement untouched and the owning claim unresolved.

This PR records ownership. It does not run the cleanup reaper, change `DeleteSession` completion, or
publish reserved fork batches. The following PR still owns atomic fork-child publication and must
document that reserved children stay hidden from List/Get until the fan-out commits.

## Compatibility

Version 10 adds the normalized resource graph and its producer in the same change. The migration is
one-way. Before upgrading, stop every daemon and embedded writer that shares the database, then back
up the database and WAL. Open the database with the v10 build only; an older binary refuses v10 on a
fresh open, but it cannot detect the migration if it already held the database open. There is no
version 10 downgrade path; restore the backup to roll back.

A v0.4.0-produced v9 fixture pins the legacy `allocations` row and `PRAGMA user_version=9`. Fresh and
migrated v10 schemas match. Every migrated or fresh inventory remains incomplete. Neither an empty
ledger, a successful fork, nor a later tracked Restore proves that historical inventory was covered.
`Store.CertifyInventory` still requires explicit reconciliation of every route, override and failed
fork. Resource confirmation and release never auto-certify inventory.
