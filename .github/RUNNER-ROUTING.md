# Runner routing variables (CHE-2425, CHE-4496)

Each job below reads its runner from a repository variable. With no variable set, the
job runs on the public default, exactly as before. The private switch sets variables
only; it moves no job in this change. `.github/tests/test_runner_routing.py` reads this
table and fails if a job in it does not use its variable with the stated default.

Set a variable: `gh variable set RUNNER_<JOB> --body <runner label>`. Unset it to return
to the public default. A value is a single runner label (`cheese-x99` matches the
self-hosted X99 runners, which carry the labels `self-hosted linux x64 cheese-x99`).

Public-repo safety (CHE-1676): a fork PR runs the workflow file from its own merge commit,
so set a variable to `cheese-x99` only after the repo is private. While it is public, keep
every variable unset.

Rules behind the targets (CHE-2425 routing table):
- `backend-test-shards` uses `services:` Postgres/Redis; the X99 runner/dind namespace
  split breaks them, so its private target is Blacksmith (`blacksmith-2vcpu-ubuntu-2404`).
  `backend-tests` is only the aggregate gate and goes to X99.
- `backend-agent-tests` and `cursor-background-lifecycle` need Linux 6.9 or newer. The X99
  host kernel is 7.0.0-38. Blacksmith ran 6.6.141 (CHE-522): do not point them there.
  Probe the kernel inside the runner container before the switch.
- Not covered (not in the approved table): `upstream-update-source-guard`,
  `locale-parity`, `frontend-quality` in `ci.yml`, and every other workflow.

| Workflow | Job | Variable | Public default | Private target |
|---|---|---|---|---|
| `ci.yml` | `warning-budget` | `RUNNER_WARNING_BUDGET` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `migration-prefix-check` | `RUNNER_MIGRATION_PREFIX_CHECK` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `changes` | `RUNNER_CHANGES` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `sqlc-check` | `RUNNER_SQLC_CHECK` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `frontend-build` | `RUNNER_FRONTEND_BUILD` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `frontend-test` | `RUNNER_FRONTEND_TEST` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `frontend-views-test` | `RUNNER_FRONTEND_VIEWS_TEST` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `backend-test-shards` | `RUNNER_BACKEND_TESTS` | `ubuntu-24.04` | Blacksmith |
| `ci.yml` | `backend-tests` | `RUNNER_BACKEND_TESTS_GATE` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `backend-agent-tests` | `RUNNER_BACKEND_AGENT_TESTS` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `cursor-background-lifecycle` | `RUNNER_CURSOR_BACKGROUND_LIFECYCLE` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `go-vulnerability-scan` | `RUNNER_GO_VULNERABILITY_SCAN` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `go-lint` | `RUNNER_GO_LINT` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `backend` | `RUNNER_BACKEND` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `frontend` | `RUNNER_FRONTEND` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `image-budget` | `RUNNER_IMAGE_BUDGET` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `installer` | `RUNNER_INSTALLER` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `script-checks` | `RUNNER_SCRIPT_CHECKS` | `ubuntu-24.04` | cheese-x99 |
| `ci.yml` | `deploy-cd` | `RUNNER_DEPLOY_CD` | `ubuntu-24.04` | cheese-x99 |
| `main-build.yml` | `daemon-artifact` | `RUNNER_DAEMON_ARTIFACT` | `ubuntu-24.04` | cheese-x99 |
| `mobile-verify.yml` | `mobile` | `RUNNER_MOBILE_VERIFY` | `ubuntu-24.04` | cheese-x99 |
| `pr-guards.yml` | `changelog-line` | `RUNNER_PR_CHANGELOG_CHECK` | `ubuntu-24.04` | cheese-x99 |
| `pr-guards.yml` | `close-target` | `RUNNER_PR_CLOSE_TARGET_CHECK` | `ubuntu-24.04` | cheese-x99 |
