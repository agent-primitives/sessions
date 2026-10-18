#!/usr/bin/env bash
# Regenerates sqlitelog/testdata/schema-v4.db only from a schema v4 checkout, through
# session.Service and HarnessRegistry over sqlitelog (see session/persisted_format_test.go).
# The writer test refuses newer schemas before touching the frozen v4 fixture.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WRITE_JOURNAL_FIXTURE=1 go test ./session -run '^TestWriteSchemaV4Fixture$' -count=1
go test ./session ./sqlitelog -run 'SchemaV4Fixture|MigratedAndFreshDatabasesMatch' -count=1
echo "wrote sqlitelog/testdata/schema-v4.db; audit it with ./hack/verify_chain.sh"
