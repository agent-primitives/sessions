# Durable allocation ownership

Placement records Create/Restore intent in SQLite before runtime I/O, then acknowledges the
returned immutable resource ID. An unresolved attempt survives failure and restart, including a runtime refusal before allocation:
the recorder cannot prove whether an error left resources behind. Only resource
identity is retained, not addresses, tokens or worker metadata. Standalone logs without the optional
recorder retain their existing behavior.

`api.CleanupRuntime` is an optional, non-destructive release contract. Local and remote backends
opt in using configuration keys; unsupported backends record an empty key instead of guessing a
safe release route. This stage does not run cleanup and does not change Fork's rollback semantics.

## Compatibility

The cleanup schema adds an allocation inventory; internal fork and cleanup columns and tables are
provisioned once for subsequent consumers. Stop **all** old daemon and embedded
writers before migration. Back up first: there is no downgrade to v0.3.0 or older after migration, and an
already-open old writer cannot be fenced by a schema stamp.

All inventories, including fresh metadata rows and implicit logs, remain incomplete. Tracking
Create/Restore alone cannot cover a subsequent legacy Fork. Neither an empty ledger, a successful
fork nor a later tracked restore establishes completeness. An operator may use
`Store.CertifyInventory` only after reconciling every namespace, runtime override and failed-fork
allocation; its evidence is persisted. Resource confirmation/release never auto-certifies inventory.
