# Merge-queue fallback (CHE-4498, from CHE-2425, parent CHE-2415)

Guards `main` without `merge_group`: required checks (not strict) plus full CI on every push to `main`. This is the mode used before 26/10/2026.

**Not applied.** The switch is an order from Cheese. Nothing in this directory runs until then. Use it when the repo goes private on GitHub Team (no merge queue there) or when the queue is turned off.

## Why three pieces

- Since CHE-3325 `ci.yml` has no `push` trigger: the queue fast-forwards `main` to a SHA its `merge_group` run already tested. Without the queue, a merge would get no CI, and deploy admission (`deploy/cd/admission.mjs`) would find no `backend`/`frontend` checks for the new SHA. The patch restores the trigger.
- A2 (CHE-2416) skips heavy jobs on push only when a green `merge_group` run exists for the same SHA. With no queue it finds none, waits up to 180 s, then runs full CI. `CI_PUSH_FULL=true` skips that lookup, so each push does not wait 3 minutes. It also makes `ci-scope.mjs decide` select every scope on a push to `main`, not only the paths changed since the previous push (`decideScopes(..., forcePushFull)`).
- The ruleset must drop its `merge_queue` rule, or PRs cannot merge (they wait for a queue that no longer runs).

## Switch, in this order

1. Set the repo variable: `gh variable set CI_PUSH_FULL --body true --repo cheese-work/multica`.
2. Merge a PR that applies `.github/fallback/ci-push-trigger.patch` (`git apply .github/fallback/ci-push-trigger.patch`). It adds `push: branches: [main]`, gives each push its own concurrency group (keyed by SHA) and sets `cancel-in-progress: ${{ github.event_name != 'push' }}`. GitHub keeps only one pending run per group, so a shared group would drop the middle SHA of three quick merges, and that SHA would have no checks for deploy admission. Update `scripts/main-build.test.mjs` in the same PR: its "no push trigger" assertion is then false.
3. Re-read the live ruleset, compare with the payload, then apply:
   `gh api repos/cheese-work/multica/rulesets/22854843 > before.json` (keep it for rollback), then
   `gh api -X PUT repos/cheese-work/multica/rulesets/22854843 --input .github/fallback/ruleset-22854843-no-merge-queue.json`.
   The payload is the ruleset read on 8/11/2026 ICT minus the `merge_queue` rule. Required checks stay `backend-tests`, `frontend`, `backend`, non-strict.

Order matters: steps 1-2 first, so `main` is never without CI between the ruleset change and the trigger.

## Rollback

`gh api -X PUT repos/cheese-work/multica/rulesets/22854843 --input before.json` (strip read-only fields: `id`, `node_id`, `source*`, `created_at`, `updated_at`, `_links`, `current_user_can_bypass`), revert the patch PR, then `gh variable delete CI_PUSH_FULL`. Merge queue needs a public repo (or Enterprise Cloud).

## Risks in this mode

- B3 locale skew (`locales/parity.test.ts`; fast check `locale-parity`, CHE-2418): two PRs, each green alone, are no longer tested together, so the skew lands on `main` and fails the push CI instead of the queue. Required checks are non-strict, so the PR does not see it. Mitigation: keep `locale-parity` in the PR run; a red push CI on `main` is fixed forward.
- A queue-only test rerun (CHE-4270, `MULTICA_TEST_RERUN_FAILS` on `merge_group`) does not run on push. A flaky test fails `main` push CI with no automatic rerun.
- `push-go-cache-warm` / `push-turbo-cache-warm` in `main-build.yml` skip (they wait for `merge_group_green`); full push CI warms the main caches itself.
- `security-gate.yml`, `queue-watch.yml` and `merge-queue-dequeue-report.yml` still mention `merge_group`; they are inert without the queue.

## Proof

`node --test scripts/merge-queue-fallback.test.mjs` checks: the patch applies and keeps push CI uncancelled; the ruleset payload keeps required non-strict checks and no `merge_queue`; with no `merge_group` run, and with `CI_PUSH_FULL=true`, A2 selects full CI for every scope even when all path filters are false; each push gets its own concurrency group.
