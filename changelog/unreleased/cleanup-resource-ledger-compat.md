---
category: Compatibility
---

- SQLite schema v10 adds allocation resource and borrower rows and is one-way. Stop every writer and
  back up the database and WAL before the first open; see
  [spec §18](../../spec.md#18-versioning-and-compatibility).
- Custom `runtime/substrate` clients must change: `ControlClient.CreateActor` returns the created
  actor's `substrate.ActorInfo` and `SnapshotCloner.TagActor` returns a `substrate.TagInfo`, each
  with the UID the server assigned. If `CreateActor` returns an empty UID, a call that
  succeeds is still recorded by session handle, as before, and one that fails after creating the
  actor stays unconfirmed. A fork needs `TagActor` to return the tag's UID; otherwise the child's
  attempt stays unconfirmed.
- A journal records resource plans only if it also implements `BeginPlannedAllocation` and, for
  fork children, `BeginUnpublishedAllocation`. A journal with only the documented
  `BeginAllocation(ctx, backend, operation)` keeps recording attempts as before, and its fork
  children fork untracked.
