# TiDB-compatible Atlas CLI release contract

The release workflow in `.github/workflows/release-atlas_oss.yaml` publishes
immutable GitHub Release assets for the Atlas community CLI.

Each release contains:

- `atlas-linux-amd64` and `atlas-linux-arm64`;
- one `.sha256` checksum file per binary;
- one keyless Sigstore `.bundle` per binary;
- the immutable Git tag as the CLI version and release identifier.

The workflow uses GitHub OIDC for keyless Cosign signing. The separate
atlas-operator change must pin a release tag and verify its checksum/signature;
it must not consume a floating `latest` asset.

## Pre-tag validation and dev releases

Pull requests use `cli-prerelease_oss.yaml` to build `dev-<sha>` binaries as
seven-day, authenticated GitHub Actions artifacts. Every push to the trusted
fork `dev` branch, or a trusted manual dispatch, publishes the current build
to the single mutable GitHub prerelease named `dev` for operator testing. The
operator must never consume `dev` in production or use a floating
`latest`/`edge`/`canary` channel there.

A manual dispatch may select any existing full commit SHA; the selected commit
is used for checkout, testing, the `dev-<sha>` version stamp, and the release
contents regardless of the dispatch ref:

```sh
gh workflow run cli-prerelease_oss.yaml \
  --ref <any-ref> \
  -f commit_sha=<full-commit-sha> \
  -f run_integration=true
```

The commit must exist in the fork. A late dispatch for an older SHA may move
the mutable `dev` channel backward intentionally, for test rollback.

Download and verify a build with:

```sh
gh run watch <run-id>
gh run download <run-id> -n atlas-cli-linux-amd64-<sha> -D /tmp/atlas-cli
cd /tmp/atlas-cli
sha256sum -c atlas-linux-amd64.sha256
./atlas-linux-amd64 version
```

For an operator-testable dev release, use the mutable asset URL and verify the
embedded commit before using it:

```sh
DEV_SHA=<commit-sha>
curl -fLO "https://github.com/esafak/atlas/releases/download/dev/atlas-linux-amd64"
curl -fLO "https://github.com/esafak/atlas/releases/download/dev/atlas-linux-amd64.sha256"
sha256sum -c atlas-linux-amd64.sha256
./atlas-linux-amd64 version # must report dev-${DEV_SHA}
```

Dev pushes/manual dispatches also publish a Cosign bundle. Verify it with the
repository and workflow identity policy before using the binary in the operator
image. Replacement deletes and recreates the complete release, so there may be
a brief 404 window but never a mixed binary/checksum set. Redispatch a desired
commit to roll back the test channel; production releases remain immutable.

Fork pull-request artifacts are unsigned because GitHub does not grant OIDC
signing to untrusted pull-request workflows. Treat them as untrusted code and
run them in a sandbox. Trusted manual dispatches publish bundles alongside both
binaries and checksums in the mutable `dev` release.

## Operator smoke test

Run these checks from the produced operator image against TiDB:

```sh
atlas version
atlas schema inspect -u "$TIDB_URL" --dev-url "$TIDB_DEV_URL"
atlas migrate diff --dev-url "$TIDB_DEV_URL" --to file://schema.hcl --dir file://migrations autorandom
atlas migrate apply -u "$TIDB_URL" --dir file://migrations
```

The dev URL must point to TiDB for replay paths that contain executable TiDB
comments. A MySQL dev database treats `/*T![...] */` as an ordinary comment and
can silently discard `AUTO_RANDOM`.
