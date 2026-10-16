#!/usr/bin/env bash
# Regenerates sqlitelog/testdata/schema-v4.db, the persisted-format fixture, from this checkout's
# session.Service and HarnessRegistry over sqlitelog (see session/persisted_format_test.go), then runs
# the tests that read it. Regenerate only for an intentional, unreleased format change: once a release
# has written schema version 4, the file is frozen and a later version gets a fixture of its own.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WRITE_JOURNAL_FIXTURE=1 go test ./session -run '^TestWriteSchemaV4Fixture$' -count=1
go test ./session ./sqlitelog -run 'SchemaV4Fixture|MigratedAndFreshDatabasesMatch' -count=1
echo "wrote sqlitelog/testdata/schema-v4.db; audit it with ./hack/verify_chain.sh"
