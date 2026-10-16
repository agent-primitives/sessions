# Journal fixtures

Checked-in sqlite journals that this build must open, read, verify and continue. Tests copy them
before opening, so the files here never change during a test run.

| File | Written by | Read by |
| --- | --- | --- |
| `v0.1.2.db` | The v0.1.2 release binaries: `agentctl exec`, `suspend`, `fork -names child-a`. Schema version 1. | `sqlitelog/harnesses_test.go`, `session/harness_registry_test.go` |
| `schema-v4.db` | This module's `session.Service` and `HarnessRegistry` over `sqlitelog`, wired as `agentsessionsd` wires them. Schema version 4. | `session/persisted_format_test.go`, `sqlitelog/harnesses_test.go` |

`hack/verify_chain.sh` also audits every `*.db` here with the independent Python verifier.

## schema-v4.db

The format the next release writes. It holds:

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
the change, or add a migration rung and keep reading the old form. Regenerate with
`./hack/gen-journal-fixture.sh` only for an intentional change to a format no release has written
yet. Once a release ships schema version 4, freeze this file and add a fixture for the next version.
