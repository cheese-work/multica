# Main push CI reuse

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
from warning collection. `daemon-artifact` still runs on main pushes.

Run the regression checks from the repository root:

```sh
node --test scripts/ci-scope.test.mjs scripts/ci-runners.test.mjs scripts/warning-budget.test.mjs
bash deploy/cd/test-record-checks.sh
bash deploy/cd/test-admission.sh
```
