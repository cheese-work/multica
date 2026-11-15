# Main push CI reuse

`ci.yml` has no `push` trigger (CHE-3325): the merge queue fast-forwards main to
the SHA its merge-group run already tested. Post-merge work runs in
`main-build.yml` on `push` to `main`. Its `merge-group` job runs
`node scripts/ci-scope.mjs merge-group-green` and the cache warmers wait for it.
The reuse text below describes the merge-group lookup that signal still uses;
`decide` still handles `push` for direct runs of older refs.

The `changes` job looks for a successful `merge_group` run of `ci.yml` for the
exact main push SHA in this repository. If found, the heavy validation jobs
skip at job level. The `backend` and `frontend` aggregate checks still run and
require those intentional skips, so deploy admission sees fresh successful
checks instead of a newer skipped aggregate.

Direct pushes without matching evidence keep normal CI. Failed, cancelled,
incomplete, stale-SHA and foreign-repository runs cannot authorize reuse.
An Actions API error or malformed response also keeps normal CI.

Set repository variable `CI_PUSH_FULL=true` to disable reuse. Unset the variable
or set it to `false` to enable reuse again. PR, merge-group, scheduled and manual
runs never reuse checks.

`push-go-cache-warm` compiles race-enabled test binaries with `-run '^$'` and
`-exec /bin/true`, without running test cases or database-initializing
`TestMain` functions. `push-turbo-cache-warm` warms build, typecheck and lint
results. Both use the existing main-branch cache keys on `ubuntu-24.04`.
The warmers do not replace validation-job warning baselines and are excluded
from warning collection. `daemon-artifact` also lives in `main-build.yml` and runs on every main push.

On pull requests the full-suite jobs follow their own path scope (CHE-4874):
`sqlc-check` runs on `sqlc` changes, `backend-test-shards`, `backend-tests`,
`go-vulnerability-scan` and `go-lint` on `backend` changes, and `frontend-build`
and `frontend-test` on `frontend` changes. The `changes` job exposes these as
`push_sqlc`, `push_backend` and `push_frontend`. `merge_group`, main pushes,
scheduled and manual runs set all three to `true`; a reused green merge group
sets all three to `false`. The `backend` and `frontend` gates map these jobs to
the same outputs, so a skipped job is expected exactly when its output is `false`.

Run the regression checks from the repository root:

```sh
node --test scripts/ci-scope.test.mjs scripts/ci-runners.test.mjs scripts/warning-budget.test.mjs scripts/main-build.test.mjs
bash deploy/cd/test-record-checks.sh
bash deploy/cd/test-resolve-candidate.sh
python3 deploy/cd/test-github-api-retry.py
bash deploy/cd/test-admission.sh
python3 .github/scripts/test_ci_wait_policy.py
python3 .github/scripts/ci-wait-policy.py
```

## No waiting while holding a runner

A job must not sleep, poll or use `--watch` for more than 120 s (CHE-5175). Wait with an environment wait timer or an issue wakeup, which hold no runner. `.github/scripts/ci-wait-policy.py` fails the check on `--watch`, on `sleep` over 120 s and on a number over 120 passed to a script whose name has watch, wait, poll, readback or observe in it. A change to any workflow, the policy file or the scanner selects the `deploy-cd` job that runs it. The scan is a heuristic over each step's `run:` text; a wait hidden in a loop or another script is for review to catch. Fixtures: `.github/scripts/test_ci_wait_policy.py`.

An exception is a line under `exceptions:` in `.github/ci-policy.yml` with `rule: inline-wait`, `workflow`, `reason`, `owner`, `root_fix` (an issue key, URL or `owner/repo#n`), `added` and `expires`. `added` is not in the future, `expires` is at most 7 days after `added` and not in the past; otherwise the exception is itself a violation. Copy the script and the CI step to another repository to apply the rule there.

## Merge-queue fallback

A ready plan, patch and ruleset payload for guarding `main` without `merge_group` live in `.github/fallback/` (CHE-4498). They are not applied.
