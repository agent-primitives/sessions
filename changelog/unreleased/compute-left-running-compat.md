---
category: Compatibility
---

- Public codes do not change. When an `Exec` or `Resume` fails after `Create` or `Restore`
  succeeded, its message gains `; compute instance "<id>" may still be running: <reason>` unless
  the journal owns the compute (`compute_state` `LIVE` from a turn or resume on the same harness,
  and no replacement instance) or the host released the compute. On a backend without
  `api.CleanupRuntime` that is every other such failure; on any backend, it is a failure where
  another owner may hold the compute (a checkpoint that superseded the turn included), the journal
  cannot say who owns it (its hash chain cannot be verified, it is shorter than when the call
  began, or a resume may have run on the call's harness or on another one), or the release fails.
  A resume counts as possibly on another harness whenever a turn with a record after the latest
  `FORK` marker ran on one (an `ExecRequest.harness` override), until the next turn record. A
  release runs under the session guard for up to 30 seconds, so other calls on that session can
  fail `ABORTED` (session busy) meanwhile; `runtime/local` and `runtime/remote` release at once.
- Under `session.WithAuthorization`, an `INTERNAL` `Exec` or `Resume` failure that left compute
  running returns `internal error; compute instance "<id>" may still be running` (with
  ` (uid "<instance>")` after the ID when the runtime returned a compute instance) instead of
  `internal error`. It carries no cause or request text; other `INTERNAL` failures still return
  `internal error`.
- `Exec` refuses a stale `expected_last_seq` before the runtime's `Create`, so a stale cursor no
  longer wakes compute. It still fails `ABORTED` (`eventlog.ErrConflict`), now with a message ending
  `expected_last_seq N, head M`, and takes precedence over later failures such as the harness
  `Describe` (spec `OPS-13`).
