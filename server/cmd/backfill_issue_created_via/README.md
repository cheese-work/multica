# backfill_issue_created_via

Sets `issue.created_via` on old issues, using the same classifier as the create path (`server/internal/issuecreatedvia`, rule set v1 of CHE-6376). Runs against the database directly (`DATABASE_URL`). It does not use the maintenance API.

## What it touches

- Domain: issues where `created_via IS NULL OR created_via = 'agent'`.
- Writes: only `issue.created_via`. Nothing else.
- Rows the classifier cannot decide stay unchanged: a row whose origin task, repeated task, trigger comment or parent no longer exists is `unresolved`. A lookup that fails for any other reason stops the run with an error and writes nothing for that batch (earlier batches stay committed and in the receipt); the create path keeps its `agent` fallback. Also: rule 7 (integration channels, reported as `unknown`) and rule 10 (no origin stamp, reported as `unresolved`).
- Release issues (`github_release_deploy.issue_id`) and CI-watch issues (metadata key `ci_watch_count`) get `autopilot`. They are counted in the `autopilot` group.

## Modes

| Mode | Command | Writes |
|---|---|---|
| Dry run (default) | `./backfill_issue_created_via` | Nothing |
| Apply | `./backfill_issue_created_via --apply --receipt <path>` | `created_via`, in batches; one receipt line per changed row |
| Revert | `./backfill_issue_created_via --revert <receipt>` | Writes `old` back only where the current value still equals `new` |

Other flags: `--workspace <uuid>` limits the run to one workspace; `--batch-size <n>` (default 500).

`--apply` and `--revert` exclude each other. `--apply` needs `--receipt`. The receipt file must not exist yet (`O_EXCL`). Apply prints `written` and `skipped`. Apply holds a session advisory lock, so a second run at the same time fails instead of racing.

## Dry-run report (JSON on stdout)

- `domain`: rows in scope.
- `counts`: rows per target value (`form`, `dm`, `dialog`, `comment`, `agent`, `autopilot`, `unknown`).
- `legacy`: per value, rows decided by rule 5b (old task with NULL `trigger_evidence_kind`).
- `unresolved`: count, plus `unresolved_issue_ids` (first 20).
- `legacy_comment_samples`: up to 5 issues decided as `comment` by rule 5b, each with `issue_id` and `trigger_comment_id`.
- `changed`: rows whose stored value differs from the decided value (what an apply would write).

## Receipt (JSON lines)

```json
{"issue_id":"<uuid>","old":null,"new":"dm"}
```

`old` is `null` when the column was NULL.

## S7 runbook (C00)

Run inside the running backend container. Do not build on C00. The binary ships in the backend image next to `backfill_task_usage_hourly`.

```bash
BACKEND=<backend container name>   # docker ps --format '{{.Names}}' | grep backend
RECEIPT=/tmp/issue-created-via-$(date -u +%Y%m%dT%H%M%SZ).jsonl   # alpine /tmp: exists, writable, not durable
```

1. Dry run. Post the JSON on S7.

   ```bash
   docker exec "$BACKEND" ./backfill_issue_created_via | tee dry-run-1.json
   ```

2. Check the stop rule. Stop, apply nothing, set S7 to blocked with the report when any of these holds:
   - `unresolved` is more than 5% of `domain`.
   - `counts.dialog + counts.dm + counts.comment` is 0.
   - A member-made issue sits in an agent group.
   - One of `legacy_comment_samples` has a trigger comment whose author is not a member (read it with `multica issue comment list`).

3. Apply with a receipt. `/tmp` in the container is not durable: a deploy or restart replaces the container and the receipt with it. Do not deploy between apply and the copy-out below.

   ```bash
   docker exec "$BACKEND" ./backfill_issue_created_via --apply --receipt "$RECEIPT"
   docker cp "$BACKEND:$RECEIPT" .    # copy out at once
   wc -l "./$(basename "$RECEIPT")"   # one line per row written; must equal `written`
   ```

4. Read back. A second dry run reports `changed` 0 and the same `unresolved`:

   ```bash
   docker exec "$BACKEND" ./backfill_issue_created_via | tee dry-run-2.json
   ```

   Then count `multica issue list --output json` (paged) by `created_via` and compare with the first report.

5. Keep the copied receipt at a recorded C00 path and attach it to S7. It is the only durable copy.

### Roll back

If the container was replaced, copy the receipt back in first:

```bash
docker cp "./$(basename "$RECEIPT")" "$BACKEND:$RECEIPT"
docker exec "$BACKEND" ./backfill_issue_created_via --revert "$RECEIPT"
```

A row changed again after the apply (current value is not the receipt's `new`) is skipped and counted, not overwritten.
