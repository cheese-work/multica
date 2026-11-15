# Fork security scan and ledger refresh

Recurring jobs for `cheese-work/multica` (CHE-3960, from CHE-3676 process v2). It does not authorize an upstream PR, issue or contact, a merge, a release or a deploy.

## Weekly security scan

Built in CHE-3701; this section names the owners.

| Item | Value |
| --- | --- |
| Scan | `.github/workflows/security-weekly.yml`: Semgrep, govulncheck, pnpm audit, gitleaks, CodeQL, Trivy. No new dependency or cost. |
| Schedule | Sunday 23:00 ICT, schedule trigger only, default branch only. |
| Maintainer | The `congvc` user on X99: holds the unseal key, runs `scripts/security-weekly/weekly.sh` from the trusted install. CODEOWNERS routes any change to the key, `seal.sh` or the workflow to `@congvc-dev`. |
| Scanned SHA | The run's `headSha`. `weekly.sh` reads it with `gh run view <run> --json headSha` and checks out exactly that commit. State it in every report. |
| Handling | Reports are sealed to the X99 key because the repo is public. Never copy finding detail to a public place: no public issue, PR, comment or log. |
| Triage owner | The assignee of the Monday morning brief autopilot (`cfc95c00-d2b4-4c31-ab14-b98614c6d89f`, step 2e), currently x99-claude-opus. It runs `weekly.sh`, which files one issue per new high or critical finding. A finding in code shared with upstream is reported privately to upstream's security contact (`triage.mjs` says so in the issue). Never a public upstream issue or PR. |

## Ledger refresh (`docs/fork/`)

Refresh the ledger at each trigger, in the same PR as the change when possible.

| Trigger | Ledger to update | Who notices |
| --- | --- | --- |
| Upstream PR candidate created (Cheese approves, scope A, once per PR) | Add the row to `UPSTREAM-PRS.md` | The author of the candidate PR |
| Review changed on a tracked PR | The row's Notes in `UPSTREAM-PRS.md` | `scripts/upstream-pr-watch.mjs` (every 15 min) wakes the row's owning issue |
| Upstream landed (merged, closed) | State and convergence PR in `UPSTREAM-PRS.md`; mark `fixed upstream` in `UPSTREAM-FIXES.md` and `UPSTREAM-OVERLAP.md` | The same watcher |
| Sync done (upstream merged into fork `main`) | Move the upstream pin and re-verify `present` rows in `UPSTREAM-FIXES.md`; refresh `FORK-FEATURES.md` | The author of the sync PR |
| Weekly | `node scripts/fork-ledger-stale.mjs`: exit 1 when upstream `main` moved from the pin, when the pin is older than 7 days or when it is missing. It also lists withheld security-class rows to re-check against upstream. | The X99 upstream watcher (`scripts/upstream-pr-watch.mjs`, every 15 min) calls the same check after each poll. The installed `run.sh` on X99 is unchanged: it already starts that file from a fresh checkout of `main`. A stale result files one issue, `chore(fork): refresh docs/fork ledger (weekly stale check)`, for the dev team; it files no second one while an issue with that title is under 7 days old (open or closed). Needs no push to the fork that week |

A stale result files that one refresh issue for the dev team. The refresh PR moves the pin.

## Upstream candidate steps (CHE-4843)

Two standing steps for every upstream PR candidate. They come from FX045 (#10111): a maintainer commit (`5ab8141e45`) added a 404 guard to our focus refetch, and the watcher never saw the PR.

1. **Failure-class pass.** Applies to any retry, refetch, poll or focus/visibility change.
   - List the transient failures (network, 5xx) and the terminal ones (404, 403, 410).
   - Grep upstream for its existing handling: `grep -rn "status === 404" packages/core` (examples: `packages/core/issues/canonical-id.ts`, `packages/core/issues/queries.ts`).
   - Add one negative test for the terminal case: it must not retry or refetch.
   - The reviewer checks this item explicitly and names it in the verdict.
2. **Track at publish.** The task that runs the publisher for a new upstream PR adds the `## Tracked` row in `UPSTREAM-PRS.md` in the same delivery, so `scripts/upstream-pr-watch.mjs` sees the PR from its first poll. No row, no publish.

## Security-class rows

A row for a defect upstream has not patched (authorization, credential or data exposure) carries the id and a one-line effect only. Evidence reads `security-class; details withheld until upstream fixes it (private report pending)`. Fill in the evidence after upstream fixes it. Redacting a row is a docs edit. Changing the fix itself is a security-class change and needs its own approval.

FX131 follows this rule since `493069753d`. Comments and tests in the code stay public.
