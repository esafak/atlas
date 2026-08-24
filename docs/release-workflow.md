# Atlas CLI release workflow

This document describes the release contract for the Atlas CLI fork. It is the
producer-side counterpart to the operator repository's
`docs/dev-release-workflow.md`, which describes how the published CLI artifacts
are pinned and consumed.

## Release channels

### Development

The `dev` branch publishes a mutable `dev` GitHub prerelease through
`.github/workflows/cli-prerelease_oss.yaml`.

Development artifacts are identified by the resolved source commit:

```text
dev-${full-40-character-sha}
```

The `dev` release is suitable for internal validation only. It must not be used
as an immutable production pin. After the signed release is published,
`.github/workflows/cli-prerelease_oss.yaml` also publishes:

```text
ghcr.io/<repository-owner>/atlas:dev-alpine
ghcr.io/<repository-owner>/atlas:sha-<full-40-character-sha>-alpine
```

`dev-alpine` is mutable. The SHA tag is immutable and is retained as one of the
five newest development image tags. Manual dispatches that publish a `dev`
release should run from `dev`; dispatching from another ref creates a release
that is not accepted by the operator's `refs/heads/dev` Cosign policy.

### Immutable production releases

An immutable release is created by pushing a `v*` tag. The tag, binaries,
checksums, Cosign bundles, and Alpine image all describe the same fork release.

The image is published by a downstream job in
`.github/workflows/release-atlas_oss.yaml`, after the signed release assets have
been published. It wraps those exact assets; it does not download or rebuild an
upstream Atlas binary.

## Versioning

Fork releases use the upstream CLI version as their base and add a
fork-qualified SemVer prerelease suffix:

```text
upstream: 1.3.1
fork:     1.3.1-esafak.1
next:     1.3.1-esafak.2
```

When the upstream base moves to `1.3.2`, the fork sequence starts at
`1.3.2-esafak.1`. Do not use a fourth numeric component such as `1.3.1.1`.

The same fork version is used for the CLI and its container distribution:

```text
Git tag:       v1.3.1-esafak.1
CLI version:   v1.3.1-esafak.1
Alpine image:  1.3.1-esafak.1-alpine
```

The `-alpine` suffix identifies the image packaging variant. The image does
not have an independent version sequence. Existing release tags and image tags
are immutable and must never be republished with different contents.

## Build and signing contract

Each immutable release publishes the following Linux assets for `amd64` and
`arm64`:

```text
atlas-linux-amd64
atlas-linux-amd64.sha256
atlas-linux-amd64.bundle
atlas-linux-arm64
atlas-linux-arm64.sha256
atlas-linux-arm64.bundle
```

The binaries are built from this repository's source and signed with Cosign.
The image job verifies both the checksum and the Cosign bundle before copying
the binaries into the image.

The immutable release workflow keeps this contract unchanged and sets
`CGO_ENABLED=0` for both architectures. Its binaries are therefore suitable
for the Alpine image and remain the production release assets.

The development workflow has two explicit consumers. `atlas-linux-amd64` is a
cgo-enabled host/test artifact for the Operator's real SQLite driver, while
`atlas-linux-arm64` remains portable. The image job never consumes either host
artifact; it consumes the additionally signed static payloads
`atlas-alpine-linux-amd64` and `atlas-alpine-linux-arm64`, each with its own
`.sha256` and `.bundle` sidecars. The image job verifies those exact payloads,
stages them under the Dockerfile's `atlas-${TARGETARCH}` contract, and runs
both architecture images against the pinned TiDB smoke target before pushing.
The static image payloads do not claim cgo-backed SQLite support.

This boundary exists because a cgo-enabled Linux binary dynamically linked to
glibc cannot execute in an Alpine/musl image. The 2026-08-24 canary exposed the
failure as `/atlas: not found` during schema preflight. CI must catch a similar
regression in the image smoke test rather than during Kubernetes
synchronization.

The canary also reported a missing `global-dev-url` secret. That is an
unrelated Fresnel deployment follow-up and is not fixed or validated by this
Atlas artifact workflow.

### Updating the development TiDB smoke image

The Alpine smoke test pulls the pinned TiDB mirror from the repository's GHCR
namespace. To update it, authenticate `crane` with a token that has
`write:packages`, copy the upstream multi-platform image, and update the
`TIDB_VERSION` default in `.github/scripts/atlas-alpine-smoke.sh`:

```sh
crane auth login ghcr.io -u esafak -p "$GHCR_TOKEN"
crane copy docker.io/pingcap/tidb:v8.5.3 \
  ghcr.io/esafak/atlas/tidb:v8.5.3
```

Verify the destination manifest before pushing the workflow change:

```sh
crane manifest ghcr.io/esafak/atlas/tidb:v8.5.3
```

## Image contract

The production image is published to:

```text
ghcr.io/<repository-owner>/atlas:1.3.1-esafak.1-alpine
```

The image is a thin Alpine wrapper around the signed release binary. Its
entrypoint and compatibility marker are:

```text
ENTRYPOINT ["/atlas"]
ATLAS_DOCKER_IMAGE=1 (compatibility marker; the current CLI does not read it)
```

OCI metadata records the fork version, source revision, and source repository.
Consumers should pin the image by digest when they require immutable behavior
within the retention window. The release workflow retains the five newest
matching SemVer `-alpine` tags by publication time, while the prerelease
workflow retains the five newest `sha-<sha>-alpine` tags and preserves `dev`
aliases. Both workflows clean untagged or orphaned GHCR manifests. Older tags
may be garbage-collected; tags are never reused with different contents.
Consumers needing longer retention should mirror the image or pin the signed
release artifacts.

## Release runbook

1. Validate the fork changes on `dev` and publish the mutable development
   artifacts.
2. Choose the next fork-qualified version according to the versioning rules
   above.
3. Push `v<upstream>-esafak.<n>`.
4. Confirm the release workflow builds and signs both architectures.
5. Confirm the image job verifies the published assets and pushes the matching
   `-alpine` image to GHCR.
6. Record or consume the binary and image digests; do not rely on mutable
   aliases.

For operator consumption, pin the release tag, source revision, architecture
hashes, and Cosign identity as described in
`atlas-operator/docs/dev-release-workflow.md`.

## Scope boundary

This document defines the CLI producer contract. It does not define operator
chart/image versioning, Kubernetes pin refreshes, or operator acceptance tests;
those remain owned by the operator repository.
