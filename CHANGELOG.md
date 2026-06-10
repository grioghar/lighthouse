# Changelog

All notable changes to Lighthouse are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Lighthouse is a maintained fork of [containrrr/watchtower](https://github.com/containrrr/watchtower).
Version 1.0.1 is the first Lighthouse release and represents the cumulative work
of taking the torch from the (no-longer-maintained) Watchtower project: security
hardening, a full rebrand, modernization, and a new health-gated rollback
feature — all while staying drop-in compatible with existing Watchtower setups.

## [1.0.1] - 2026-06-10

### Added
- **Health-gated updates with rollback.** With `--health-gated`
  (`WATCHTOWER_HEALTH_GATED` / `LIGHTHOUSE_HEALTH_GATED`), Lighthouse waits up to
  `--health-timeout` (default `60s`) for an updated container to report a healthy
  Docker `HEALTHCHECK` — or, for images without one, to stay running without
  crash-looping. If it doesn't become healthy, the container is stopped and
  **recreated from the previous image**, and the update is reported as failed so
  the previous image is preserved. Protects services from bad releases.
- **Root `Dockerfile`** so `docker build https://github.com/grioghar/lighthouse.git`
  and a Compose `build:` stanza pointing at the repo work without specifying a
  Dockerfile path.
- **`LIGHTHOUSE_*` environment variables** and **`lighthouse.*` container labels**
  as the new canonical names, with the legacy `WATCHTOWER_*` variables and
  `com.centurylinklabs.watchtower.*` labels honoured as fallbacks (the new form
  wins when both are set).
- **`NOTICE.md`** and README attribution crediting the original Watchtower
  authors and CenturyLink Labs.
- New monogram-badge **logo** and favicons.

### Changed
- **Rebranded** the project from Watchtower to Lighthouse: Go module path
  (`github.com/containrrr/watchtower` → `github.com/grioghar/lighthouse`), binary
  and command name, user-facing logs, notification sender/title, Dockerfiles,
  Compose, goreleaser, and CI references.
- **Docker API version negotiation.** The client now negotiates a mutually
  supported API version with the daemon (and the pinned default moved `1.25` →
  `1.40`), fixing crash-loops against modern daemons that reject old API
  versions (`client version 1.25 is too old. Minimum supported API version is
  1.40`).
- **Toolchain:** Go `1.20` → `1.25`; `golang.org/x/net` `0.19.0` → `0.55.0`;
  replaced the deprecated `golang.org/x/net/context` with the standard library
  `context`.
- **CI:** test/lint/build on Go `1.25.x`; staticcheck `2026.1`; GitHub Actions
  bumped (`checkout` v5, `setup-go` v5, `codecov-action` v5) and opted into the
  Node 24 runtime. Dockerfile base images pinned to `golang:1.25-alpine`. The
  production release workflow skips image publishing when registry secrets are
  absent, so tagging a release no longer fails on forks.

### Security
- **Registry TLS verification restored** — removed `InsecureSkipVerify` from the
  registry digest client, which had trusted any certificate while sending
  registry credentials (MITM / credential-exposure risk).
- **HTTP API hardening** — constant-time API-token comparison, a dedicated
  `ServeMux` with read/write/idle timeouts (Slowloris hardening), a bounded
  request body on the update endpoint (removed an unbounded copy to stdout), and
  a closed/handled registry auth response body.
- **Efficiency:** the registry digest client is reused across a scan instead of
  being reallocated per request.

### Removed
- A 20 MB build artifact (`oryxBuildBinary`) that had been committed to the repo.

---

## Attribution

The foundation of this project is **Watchtower** by the Watchtower contributors
and CenturyLink Labs, licensed under the Apache License 2.0. Lighthouse retains
that license and is grateful for their work. See [NOTICE.md](NOTICE.md).

[1.0.1]: https://github.com/grioghar/lighthouse/releases/tag/v1.0.1
