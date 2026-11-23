---
name: verify-multica
description: Use when you must see a change to the Multica fork working in the real app on X99 (web UI, decision card, notification settings, workflow status) and attach proof, not only run tests.
---

# verify-multica

Drives this fork's own app on X99 in a real browser and leaves evidence. Build and drive on X99 only. Never launch on C00. Never build Docker images here.

Evidence rule: a row with no screenshot or log from this run is "not verified". Never put a token, passkey or personal data in a log or screenshot; the seeded stack uses throwaway users.

Helpers live in `.agents/skills/verify-multica/scripts/` and run from any directory.

## Launch

1. Run `.agents/skills/verify-multica/scripts/launch.sh`.
2. Read the last line it prints: `run_dir=... api=... web=... db=...`. Keep it; Cleanup needs nothing else.

What it does: refuses a checkout that already has a registered environment (it is not yours; use a fresh checkout), records an ownership record (run id, environment name, creation time) in `~/.multica/verify-multica/`, then runs `make up` with `--ephemeral --ttl 2` (own ports, own database `multica_<n>`, own CLI profile), retries the web start, and warms `/login` and the chat route. Every process it starts carries `VERIFY_MULTICA_RUN_ID`. First run on a loaded host: `pnpm install` about 5 minutes, first page compile about 2 minutes. Budget 10 minutes.

## Doctor

1. Run `.agents/skills/verify-multica/scripts/doctor.sh`.
2. Every line must read `OK`. Any `FAIL` stops the run; fix that line first.

Read-only. Checks: host is X99, Playwright and Chromium installed, environment registered, API `/health`, web `/login`, and that the database is the isolated one, never the shared `multica`.

## Drive

1. Run `.agents/skills/verify-multica/scripts/drive.sh <evidence-dir>`. Pick a directory outside the run dir (for example `../evidence`) so Cleanup cannot remove it.
2. Exit 0 means every assertion passed.

Current driver, `scripts/decision-card.spec.ts`: seeds a refused weekly run (the `weekly_run_guard_refusal` row), has an agent propose `issue.run_guard.allow`, opens the DM, and asserts the card shows "Weekly run limit" with Approve enabled (the CHE-6242 assertion). The refusal is seeded; it does not trip the guard with 30 real runs. The page is served through an https origin with a virtual authenticator, as `e2e/decision-dm-card-renders.spec.ts` does.

Expect about 4 minutes on a loaded host. To drive another feature, read its file in `features/` first and add a spec next to `decision-card.spec.ts`.

## Evidence

Files `drive.sh` leaves in `<evidence-dir>`:

- `decision-card-run-guard.png`: the card, with "Weekly run limit 30 → 40" and Approve/Reject.
- `doctor.log` and `drive.log`: the step log and run transcript (STEP lines carry UTC times).
- `report.json`: Playwright's result.

1. Upload each file to the issue: `multica attachment upload <file>` or `--attachment` on the comment.
2. In the comment, give the app head SHA (`git rev-parse HEAD`), host, ICT time of the run, and `df -B1 /mnt/ssd2` before and after.
3. Never link a local path. Say "attached".

## Cleanup

1. Run `.agents/skills/verify-multica/scripts/cleanup.sh`.
2. Read `port N closed` for the api and web ports. A failed port check or database drop exits non-zero.

It acts only on the environment its ownership record names: it refuses (exit 3) with no record, a non-agent owner, a non-`multica_*` database, or a name, creation time or database that differs from the record. It stops only pids whose cwd is inside this checkout and whose environment carries this run's `VERIFY_MULTICA_RUN_ID` (`scripts/stop-pid.sh`), never by name: the daemon is also called `multica`, and another process in the same checkout is left alone. Evidence outside the run dir stays. Remove nothing else; record `df -B1` after.

## Features

Per-feature maps (what it does, how to reach it on web, desktop, CLI and mobile, settings, states, edge cases, ledger row) are in `features/README.md`. Seeds: decision cards, notification settings, workflow status. Mobile needs a device; this skill does not give that evidence.

## Tests

`node --test .agents/skills/verify-multica/scripts/*.test.mjs` pins the sections, helpers, feature index and the stop-pid rule (`skill.test.mjs`), and launch/cleanup ownership with a fake `make` (`ownership.test.mjs`): a pre-existing environment is refused and untouched, an unlaunched or replaced environment is not destroyed, and only this run's processes are stopped. `VERIFY_MULTICA_SKIP_HOST_CHECK=1` exists for those tests only.
