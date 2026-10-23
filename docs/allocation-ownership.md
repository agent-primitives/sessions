# Durable allocation ownership

Placement records Create/Restore intent in SQLite before runtime I/O, then acknowledges the
returned immutable resource ID. An unresolved attempt survives failure and restart, including a
runtime refusal before allocation: the recorder cannot prove whether an error left resources behind.
Only the returned handle is retained, not addresses, tokens or worker metadata. Runtimes must return
a nonempty `Incarnation.ID` from Create/Restore regardless of the log implementation. SQLite-tracked
placement enforces this requirement: an empty ID fails confirmation with
`sqlitelog.ErrAllocationConflict` before dialing and leaves the attempt unresolved. The handle is
not the compute-instance identity used to detect replacement. Standalone logs without the optional
recorder retain their existing behavior.

Once the runtime returns a handle, confirmation uses a separate five-second timeout, independent
of client cancellation. A failed confirmation leaves the durable attempt unresolved. Successful
confirmation atomically retires prior confirmed, unreleased attempts with the same session, backend
key and resource ID, keeping the attempt being confirmed. Repeated Execs and Resumes that reattach
to the same resource therefore retain one live row, not one per turn. Unknown outcomes, distinct
resource IDs or sessions, distinct nonempty backend keys and released history are preserved;
repeated failures can still grow the ledger. Backends without `api.CleanupRuntime` record an empty
key: different key-less configurations returning the same resource ID for the same session compact
into one row. These rows have no safe release route. This is resource inventory, not a per-call
audit log.

Repeating confirmation of a retained attempt with the same resource ID is harmless, including after
release. Confirming or releasing a compacted attempt returns `sqlitelog.ErrAllocationNotFound`. If a
late confirmation retires a row already paged for cleanup, reread the live inventory and retry its
idempotent release; the surviving row remains unreleased.

`api.CleanupRuntime` is an optional, non-destructive release contract. Local and remote backends
opt in using configuration keys; unsupported backends record an empty key instead of guessing a
safe release route. This stage does not run cleanup and does not change Fork's rollback semantics.

## Compatibility

Version 7 adds an allocation inventory; internal fork and cleanup columns and tables are
provisioned once for subsequent consumers. Stop **all** old daemon and embedded
writers before migration. Back up first: there is no downgrade to v0.3.0 or older after migration, and an
already-open old writer cannot be fenced by a schema stamp.

All inventories, including fresh metadata rows and implicit logs, remain incomplete. Tracking
Create/Restore alone cannot cover a subsequent legacy Fork. Neither an empty ledger, a successful
fork nor a later tracked restore establishes completeness. A host embedding `sqlitelog` may call
`Store.CertifyInventory` only after reconciling every namespace, runtime override and failed-fork
allocation; its evidence is persisted. Resource confirmation/release never auto-certifies inventory.
