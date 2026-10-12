# C00 deployment queue health

`cd-queue-watch.yml` runs a read-only alarm on a GitHub-hosted runner every
15 minutes (GitHub schedules can be delayed) and on manual dispatch. It has
no deploy concurrency group, production environment, C00 secrets, or write
permissions. A queue cannot hide its own failure behind its unavailable runner.

A current-attempt `deploy` job in `queued`, `pending`, or `waiting` for at least
30 minutes fails the alarm with the exact job URL. Age starts at job creation
(or its API `started_at` when creation is absent), never original workflow
creation. Old attempts and long preparation do not age a newly queued deploy.
Runs without a deploy job yet, including workflow-level concurrency waits, are
not timed by this alarm. Environment waits represented by a deploy job do alert.
Running deployments are not interrupted. All API pages for active runs and
their current-attempt jobs are read; API or malformed-data failures fail
loudly rather than claiming health. This is detection, not automatic recovery
or a hard queue deadline. GitHub job `timeout-minutes` does not bound runner
queue time. Enable Actions failure notifications for this workflow; schedule
activation requires merging it to the default branch.

## Safe diagnosis and recovery boundary

1. Read the affected run and all jobs with `gh api
   repos/cheese-work/multica/actions/runs/<run-id>/jobs`. An empty runner name,
   runner ID 0, and no steps mean the deploy script has not started.
2. Inspect the actual deploy runner on X99: `docker inspect
   cheese-c00-deploy-runner-1` with selected state fields only, and bounded
   `docker logs`. Never print its environment, Compose config, or credentials.
   Repo runner inventory can be empty for org-scoped runners; that is not proof
   of absence.
3. During App-key rotation, reconcile **every** consumer, including deploy and
   OCR runners, not just build lanes. An ephemeral runner can finish its current
   job with its existing session and then fail re-registration using stale
   in-memory credentials. Updated source files do not refresh existing container
   environments. Prove fresh App authentication and provider online/busy status.
4. Before refreshing a deploy runner, reconcile queued release candidates,
   baseline freshness, environment approval, and production authorization with
   the operational owner. Bringing the runner online can immediately execute
   an admitted production deployment. Do not automatically restart, reroute,
   cancel, or dispatch as a queue-health repair.

## Reproducible verification

On X99, from the repository root:

- `python3 deploy/cd/test-check-deploy-queue.py` verifies queue states, threshold,
  duplicate/page handling, fail-loud API errors, and independent read-only wiring.
- `GITHUB_REPOSITORY=cheese-work/multica python3 deploy/cd/check-deploy-queue.py`
  reads the real queue using ambient `gh` authentication. Exit 1 with run links
  means a stalled queue was observed, not a failed test or deployment attempt.

The root-cause runner configuration must be repaired by its X99 owner under
production admission controls. This repository alarm does not replace that fix
or guarantee that a runner never becomes unavailable.
