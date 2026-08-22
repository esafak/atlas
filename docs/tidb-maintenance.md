# TiDB fork maintenance

This document describes the small, rebase-friendly TiDB compatibility surface
and the checks required before publishing a fork artifact.

## Branch and commit flow

- Keep TiDB work on `feature` branches and promote it through `dev` to `main`.
- Rebase the feature branch onto upstream Atlas `master` before promotion.
- Keep these changes in separate, independently buildable commits:
  1. corpus fixtures and TiDB 8 integration/CI wiring;
  2. TiDB inspection, schema/HCL conversion, and unit tests;
  3. SQL generation, diff guards, and integration tests;
  4. release and maintenance process documentation.

## Rebase checks

After each upstream `master` update, run:

```sh
go test ./sql/mysql -count=1
go test ./sql/migrate -count=1
go test ./sql/internal/specutil ./sql/internal/sqlx -count=1
go test ./cmd/atlas/... -count=1
cd internal/integration
docker compose --project-name atlas-integration up -d tidb5 tidb6 tidb8
go test -run '^TestTiDB_' -version=tidb5 ./...
go test -run '^TestTiDB_' -version=tidb6 ./...
go test -run '^TestTiDB_' -version=tidb8 ./...
go test -run '^TestTiDB_Script$' -version=tidb5 ./...
go test -run '^TestTiDB_Script$' -version=tidb6 ./...
go test -run '^TestTiDB_Script$' -version=tidb8 ./...
```

The TiDB matrix uses `tidb5`, `tidb6`, and `tidb8` as test keys. The image
pins are `v5.4.0`, `v6.6.0`, and TiDB Cloud-aligned `v8.5.3`.

Every push to the fork's `dev` branch runs the pre-release workflow, including
the live matrix, and refreshes the mutable operator-test `dev` prerelease. To
rerun it manually before creating an immutable release tag:

```sh
gh workflow run cli-prerelease_oss.yaml -f run_integration=true --ref <branch>
gh run list --workflow cli-prerelease_oss.yaml --limit 1
gh run watch <run-id>
gh release view dev
```

To test an exact commit instead of the selected ref head, provide its full
40-character SHA:

```sh
gh workflow run cli-prerelease_oss.yaml \
  --ref <any-ref> \
  -f commit_sha=<full-commit-sha> \
  -f run_integration=true
```

The commit must exist in the fork. If checkout reports that it cannot fetch
the commit, push it to a branch first. A late dispatch for an older commit may
intentionally move the mutable `dev` test channel backward.

Pull-request artifacts are unsigned and must be treated as untrusted code;
verify their checksum and run them sandboxed. A trusted manual dispatch or
`dev` push refreshes the mutable `dev` prerelease by deleting and recreating it.
Roll back the test channel by redispatching the desired commit; production
rollbacks still restore the previous immutable release pin.

## Conflict hotspots

Keep the TiDB behavior isolated in `sql/mysql/tidb.go`. The shared edit points
are the column attribute loops in `sql/mysql/migrate.go` and
`sql/mysql/sqlspec.go`; integration and CI version matrices, scanner tests, and
release packaging are mechanical conflict points. Avoid changing shared
`inspect.go` and `diff.go` for this feature.

The pre-release workflow is another mechanical hotspot. Keep it separate from
the generated `ci-*` workflows and from the immutable release workflow. Its
publish job must remain limited to trusted `dev` pushes/manual dispatches and
must replace the single mutable dev release as a complete asset set.

## Artifact rollback

Artifacts are immutable GitHub Release assets. If a release is bad, pin the
operator build back to the previous verified release and leave the canonical
`mysql://` URL and driver selection unchanged. Do not replace or mutate an
existing release asset.

This immutable rollback rule applies to production `v*` releases. The separate
operator-test `dev` channel is intentionally mutable and is rolled back by
redispatching the desired commit.
