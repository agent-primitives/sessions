# Journal fixtures

Checked-in sqlite journals that this build must open, read, verify and continue. Tests copy them
before opening, so the files here never change during a test run.

| File | Written by | Read by |
| --- | --- | --- |
| `v0.1.2.db` | The v0.1.2 release binaries: `agentctl exec`, `suspend`, `fork -names child-a`. Schema version 1. | `sqlitelog/harnesses_test.go`, `session/harness_registry_test.go` |
| `approval-v3.db` | The unreleased schema version 3 approval store, through its public API: empty, parked and decided-without-receipt sessions, a retired registration and a reserved static route. Schema version 3. | `sqlitelog/deletion_v3_migration_test.go` |
| `schema-v4.db` | This module's `session.Service` and `HarnessRegistry` over `sqlitelog`, wired as `agentsessionsd` wires them. Schema version 4. | `session/persisted_format_test.go`, `sqlitelog/harnesses_test.go` |
| `approval-v7.db` | The schema version 7 build at `d5a98c80`, through `sqlitelog.Open` and `Session(...).Append`: `decided` (a gated call, a decision without a verified approver, its receipt and END) and `parked` (a gated call waiting for a decision). Schema version 7. | `sqlitelog/verified_approver_schema_test.go` |
| `cleanup-v9.db` | The v0.4.0 tag (`1d5d97c`, Go public `Open`, `BeginAllocation` and `ConfirmAllocation` APIs), with one confirmed legacy handle allocation. Schema version 9. SHA-256 `b260f96ca958ae5bf6d486f91881718346b3a2662c59a410baa92b023ff31700`. | `sqlitelog/cleanup_identity_migration_test.go` |

Regenerate `cleanup-v9.db` by copying `hack/gen-cleanup-v9-fixture.go` into a detached v0.4.0
worktree and running `go run ./hack/gen-cleanup-v9-fixture.go -mode generate -out cleanup-v9.db`.
The helper asserts both `PRAGMA user_version=9` and a nonempty allocations table. Its `open` and
`hold` modes exercise the one-way downgrade refusal and the operational already-open-writer limit.

`hack/verify_chain.sh` also audits every `*.db` here with the independent Python verifier, and
`internal/fuzzcheck.JournalEvents` seeds the fuzz targets that decode an Event from their records.

## schema-v4.db

The frozen schema version 4 format. It holds:

- `parent` on `echo`: two turns, Suspend and Resume, with identity, origin, labels and annotations.
- `child`: a fork of `parent` at its head, plus one turn of its own.
- `gate` on a test harness: a `REQUIRES_APPROVAL` tool call whose arguments cover every Struct value
  shape, the park, an approval with reason and identity, the tool receipt, a model call and usage.
- `deleted` on `echo`: one turn, then `DeleteSession`, so the session is a PENDING tombstone whose
  journal ends in the DELETE record.
- A registered remote harness, `remote-echo`, then retired.

`TestSchemaV4FixtureReadsReplaysAndContinues` checks that each chain verifies, that each record
decodes to what was written, that every live session replays from recorded effects without calling
the model or the tool executor, that the registry row's digest is still the digest of its canonical
spec, that the deleted session stays hidden from List, reports its request time and refuses a new
turn, and that a new turn on a live session appends. `TestMigratedAndFreshDatabasesMatch` checks
that, after `Open`, its schema is the schema a fresh database gets.

A failure means this build changed how it stores or encodes something a journal already holds. Fix
the change, or add a migration rung and keep reading the old form. The generator
`./hack/gen-journal-fixture.sh` refuses to overwrite this fixture from a newer-schema checkout;
a newer format needs a fixture of its own.
