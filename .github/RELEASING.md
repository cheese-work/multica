# Release runbook

## Normal release

Release from a reviewed commit on `main` by creating and pushing a new semantic
version tag such as `v0.18.4`. The Release workflow intentionally has no manual
trigger: a tag push is the only event that can publish binaries, Homebrew
formulae, and container images.

The verification job runs the Go tests and `govulncheck` before any publishing
job starts. The vulnerability scan is fail-closed by default.

Release notes use GoReleaser's GitHub changelog to credit commit authors and
recognized co-authors between the previous and current tags. Keep all commits,
including documentation, tests, and chores, so their authors are credited too.
Commit-subject `@` characters are separated from the following text with an HTML
comment; only the generated author mentions populate GitHub's Contributors list.

CI and release jobs require Go `~1.27.4`: at least 1.27.4, while accepting
newer 1.27 patches. This security floor is independent of the development
minimum in `server/go.mod`. A plain `1.27.x` can resolve to an older patch
while the `setup-go` version manifest catches up with an official Go release.
When raising the security floor, update all Go workflows and the pinned
Go builder image in `Dockerfile` together so shipped binaries also receive
the fixes.

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
<<<<<<< ours

## Release build stamps

`UPSTREAM_VERSION` at the repository root tracks the latest synced plain upstream
semver (without a `v` prefix, for example `0.6.3`). Every upstream sync PR updates
`UPSTREAM_VERSION` to the synced upstream tag.

`scripts/release-stamp.sh` formats a timestamp in ICT (`Asia/Ho_Chi_Minh`, UTC+7)
as `YYYYMMDD-hhmm`. When given a commit ISO timestamp (`git show -s --format=%cI`),
it derives the stamp deterministically from that commit time; with no timestamp
argument it falls back to the current ICT time.

The verification job and CD qualification build compute the ICT commit-time stamp
from `scripts/release-stamp.sh` and read the plain upstream semver from
`UPSTREAM_VERSION`:
- Backend Docker builds pass `VERSION=<upstream>`, `BUILD=<stamp>`, and
  `COMMIT=<sha>`.
- Web Docker builds pass `NEXT_PUBLIC_APP_VERSION=<upstream>`,
  `NEXT_PUBLIC_APP_BUILD=<stamp>`, `NEXT_PUBLIC_APP_COMMIT=<sha>`, and
  `NEXT_PUBLIC_BUILD_DATE=<commit-iso>`.
- Daemon/CLI artifacts and GoReleaser stamp `<upstream>-<stamp>` into
  `main.version` so `buildinfo.Split` separates `version` and `build`, while
  keeping the commit SHA in `main.commit` / `main.buildCommit`.
- Existing semantic-version, `sha-<GITHUB_SHA>`, and stable `latest` image tags
  stay unchanged.

The joined `<upstream>-<stamp>` string is display-only:
- `multica version --output json` returns plain `version`, separate `build`, and
  `display_version`.
- Self-hosted `/api/config` returns separate `server_version` (plain upstream
  semver), `server_build` (`YYYYMMDD-hhmm`, omitted when unstamped), and
  `server_commit` (7-char short SHA, omitted when unknown). The Help popover joins
  `server_version` and `server_build` for display (`0.6.3-20261021-0856`, or plain
  `0.6.3` when `server_build` is absent) and hides the row when `server_version`
  is empty. Official cloud config omits all three server build fields.

Runtime update comparisons accept release stamps and `+` metadata, but do not
treat git-describe, dirty, or prerelease versions as stable releases. CLI
minimum-version gates retain their existing parsing and dev-build policy.
Local builds, PR artifacts, release tags, and archive names are unchanged.
For an unpublished GoReleaser snapshot, leave `RELEASE_BUILD_STAMP` unset; for
a release outside the workflow, set `UPSTREAM_VERSION` and `RELEASE_BUILD_STAMP`
before running GoReleaser.
||||||| base
=======

## Stable release promotion

GoReleaser publishes a public release with `make_latest: false`. Installed stable
clients and CLI entry points that read Latest keep using the previous complete
version while Desktop artifacts are uploading. Homebrew continues to publish
normally because CLI archives are already public; container tags are independent.
The release page and GitHub notifications can show the new release before promotion.

`.github/workflows/promote-release.yml` is the only entry point for changing
Latest. The Linux/Windows Desktop matrix calls it after both builds succeed.
After the manual macOS release has signed, notarized, verified, and uploaded both
architectures, the release-desktop skill dispatches the same workflow:

```bash
gh workflow run promote-release.yml --repo multica-ai/multica --ref main -f tag=vX.Y.Z
```

The workflow uses the reviewed validator from `main`, not executable code from the
requested tag. It serializes promotion globally and only promotes a stable
semantic version newer than the current Latest. Repeating a completed promotion
or finishing an older release after a hotfix leaves Latest unchanged.

The gate checks all six update metadata files (`latest-mac.yml`,
`latest-x64-mac.yml`, `latest.yml`, `latest-arm64.yml`, `latest-linux.yml`, and
`latest-linux-arm64.yml`), their version against the tag, and each referenced
artifact's architecture, upload status, and byte size. It also requires all six
CLI targets in both legacy and versioned archive names plus `checksums.txt`.
macOS update metadata must reference the ZIP; Windows requires EXE and Linux
AppImage. Missing blockmaps do not block promotion. The macOS `afterSign` hook
runs `spctl` and `stapler validate` after notarization and before packaging/upload
for explicit publishing builds. The release skill requires code signing so an
unsigned build cannot skip that hook. These host checks do not run for unsigned
local builds or other platforms. The gate does not download large binaries to
recalculate SHA-512.

Missing or inconsistent assets produce a `waiting` result with a list of blockers
and leave Latest unchanged. This is normal when macOS and CI finish in either
order. The next completed upload path tries again. API/authentication failures
fail the job. Inspect the promotion run result; a successful dispatch only means
the run was queued. If needed, correct the failed upload and dispatch the workflow
again. Do not bypass the gate with a direct `gh release edit --latest`.

Land this workflow on `main` before the first release using `make_latest: false`.
Until promotion succeeds, stable clients stay on the previous version. Existing
clients need no changes to benefit from the release ordering fix; friendly failure
messages and download recovery require the newly shipped Desktop client.

For a faulty release, publish a higher patch version through the same gate.
Do not move Latest backwards: already installed Windows arm64 clients predating
the downgrade guard may install the older binary. Removing `make_latest: false`
reintroduces the incomplete-release window and is not a version rollback.
>>>>>>> theirs
