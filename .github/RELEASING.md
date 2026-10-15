# Release runbook

## Normal release

Release from a reviewed commit on `main` by creating and pushing a new semantic
version tag such as `v0.18.4`. The Release workflow intentionally has no manual
trigger: a tag push is the only event that can publish binaries, Homebrew
formulae, and container images.

The verification job runs the Go tests and `govulncheck` before any publishing
job starts. The vulnerability scan is fail-closed by default.

## Emergency vulnerability-scan bypass

Use the bypass only when `govulncheck` itself or its live vulnerability database
is unavailable, or when maintainers have documented a confirmed false positive
that blocks an urgent release. Never use it to publish a release with an
unresolved reachable vulnerability.

1. Record the reason and maintainer approval in the release issue or pull
   request, and confirm no other release is in progress.
2. In **Settings → Secrets and variables → Actions → Variables**, set the
   repository variable `ALLOW_VULN_BYPASS_FOR_TAG` to the exact release tag,
   for example `v0.18.4`.
3. Re-run the failed Release workflow for that tag. A different tag, an empty
   value, or any typo keeps the scan enabled.
4. Confirm the verification log contains the explicit bypass warning and retain
   the workflow URL in the incident record.
5. Delete `ALLOW_VULN_BYPASS_FOR_TAG` immediately after the release run
   completes. The tag-scoped value prevents a concurrent release with another
   tag from inheriting the bypass.

Every Go binary retains its compiler version in the standard Go build metadata;
use `go version -m <binary>` when auditing a downloaded release artifact.

## Release build stamps

The verification job computes one ICT (`Asia/Ho_Chi_Minh`, UTC+7) stamp in
`YYYYMMDD-hhmm` format. The publishing jobs reuse that output for CLI ldflags,
backend and web image builds, and an additional stamped image tag. Existing
semantic-version, SHA, and stable `latest` image tags stay unchanged.

`scripts/release-stamp.sh` is the byte-identical shared helper from
`cheese-work/multica-dotfiles` at published revision
`5dafb3d959466df589353a8de1da0def15102a62` (CHE-1308). This pinned vendored copy
lets release jobs call the shared implementation without adding a credential
for cross-repository access to that private repository. Refresh the copy and
its checksum test together when the shared helper changes.

The joined `<upstream>-<stamp>` string is display-only. `multica version
--output json` returns plain `version`, separate `build`, and `display_version`.
Self-hosted `/api/config` adds `version` and `build` while retaining
`server_version` as the display string for installed clients. Official cloud
config still omits server build information.

Runtime update comparisons accept release stamps and `+` metadata, but do not
treat git-describe, dirty, or prerelease versions as stable releases. CLI
minimum-version gates retain their existing parsing and dev-build policy.
Local builds, PR artifacts, release tags, and archive names are unchanged.
For an unpublished GoReleaser snapshot, leave `RELEASE_BUILD_STAMP` unset; for
a release outside the workflow, set it once with
`export RELEASE_BUILD_STAMP="$(sh scripts/release-stamp.sh)"` before GoReleaser.
