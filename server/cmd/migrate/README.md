# Migration runner operations

## Squad leader failure marker renumbering

Migration `614_squad_leader_failure_marker` replaces the later-merged
`593_squad_leader_failure_marker` filename to remove its numeric-prefix
collision with `593_chat_continuation`. The chat-continuation migration is
unchanged. Numbers 594 through 613 are reserved by the channel-origin stack;
none of those migrations or the draft chat-auto-compaction migration replaces
`capture_issue_collaboration_wakeup`.

Migration versions are file basenames, not numeric prefixes. A database that
already recorded `593_squad_leader_failure_marker` therefore runs the identical
SQL again and records `614_squad_leader_failure_marker` as a new version. Leave
the historical version row intact. The up migration contains only
`CREATE OR REPLACE FUNCTION`, so reapplication preserves the function identity
and existing trigger while continuing to exclude `squad_leader_failed` from
collaboration wakeup capture. No migration-runner name map entry is needed.

The down migration restores capture of marker changes; reapplying the up
migration suppresses them again. Application rollback alone does not require
rolling back this function. Production deployment remains gated on Cheese.

## Prebuild the chat-session delete lookup index

Migration 472 builds `idx_agent_task_queue_chat_session`, which keeps the
legacy `chat_session` foreign-key delete action from scanning the global task
queue for every deleted chat. Its ordered keys also continue to cover
`AdvanceCancelledChatSessionPointer` after migration 473 removes the narrower
migration 465 index. The replacement includes every task attached to a chat,
so measure the eligible population before scheduling the build:

```sql
SELECT count(*) AS indexed_rows
FROM agent_task_queue
WHERE chat_session_id IS NOT NULL;
```

Migrations run during backend startup, whose Helm startup probe allows ten
minutes. On a large production table, check for long-running transactions and
prebuild the index in a low-traffic window. Run the statement by itself and
outside a transaction:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_chat_session
ON agent_task_queue (chat_session_id, created_at DESC)
WHERE chat_session_id IS NOT NULL;
```

Confirm the build is usable before deploying:

```sql
SELECT indexrelid::regclass AS index_name,
       pg_size_pretty(pg_relation_size(indexrelid)) AS size,
       indisvalid,
       indisready,
       indislive
FROM pg_index
WHERE indexrelid = to_regclass('idx_agent_task_queue_chat_session');
```

Migration 472 then becomes a fast no-op. Migration 473 only drops the narrower
index concurrently and does not scan `agent_task_queue`; migration 474 builds a
single-column index on `dingtalk_bot_identity`, not the global task table. If an
interrupted manual build leaves the replacement index invalid, drop that invalid
index concurrently and retry the standalone build before deploying. The
migration runner also registers invalid-index cleanup so an interrupted startup
build can recover on its next attempt.

Application rollback is compatible with the extra index, so leave it in place.
A full database rollback restores migration 465's narrower index before dropping
the replacement, but returns chat-session deletes to the global task-table scan.

## Issue description search index retirement

Migrations 463 and 464 retire the two historical issue-description search
indexes: `idx_issue_description_bigm` and `idx_issue_description_trgm`.
`SearchIssues` now scans the selected workspace's issue candidates and evaluates
description matches during projection, so neither global description GIN is
read. Fresh installs also skip migration 139's historical trigram build. Do not
repair or recreate these indexes after applying migrations 463 and 464.

The down migrations restore the trigram index everywhere and also restore the
CJK-friendly bigram index where `gin_bigm_ops` is available. Both use
`CREATE INDEX CONCURRENTLY` and retry safely after an interrupted build.

These migrations run during backend startup. A concurrent drop can wait for old
transactions, so for multi-gigabyte production indexes prefer a low-traffic
window: check for long-running transactions, then run each statement separately
and outside a transaction before deploying:

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_description_bigm;
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_description_trgm;
```

The subsequent migrations become fast no-ops. If startup performs a drop and
is interrupted, `IF EXISTS` makes the next run retry safely; one index may
remain until that retry completes. Rolling back to the current candidate-first
search remains functionally correct without either index, but rolling back to
the legacy search can make description queries much slower or time out until
the database rollback rebuilds the indexes. Rebuilding up to two multi-gigabyte
GIN indexes is not immediate; verify every restored index is live, ready, and
valid before relying on it:

```sql
SELECT indexrelid::regclass AS index_name, indisvalid, indisready, indislive
FROM pg_index
WHERE indexrelid IN (
    to_regclass('idx_issue_description_bigm'),
    to_regclass('idx_issue_description_trgm')
);
```

## Comment content search index retirement

Migrations 454 and 455 retire both historical comment-content search indexes:
`idx_comment_content_bigm` on pg_bigm deployments and the portable
`idx_comment_content_trgm` fallback. `SearchIssues` now scans comments through
`idx_comment_workspace` and evaluates content matches during aggregation, so it
no longer reads either global content GIN. Fresh installs also skip migration
140's historical fallback build. Do not repair or recreate these indexes after
applying migrations 454 and 455.

The down migrations restore exactly one historical index with
`CREATE INDEX CONCURRENTLY`: migration 454 restores the pg_bigm index where
`gin_bigm_ops` is available, while migration 455 restores the pg_trgm fallback
everywhere else.
Both restore the original `LOWER(content)` expression and retry safely after an
interrupted concurrent build.

These migrations run during backend startup. A concurrent drop waits for old
transactions, and the Helm startup probe allows ten minutes before restarting
the pod. For the multi-gigabyte production index, prefer a low-traffic window:
check for long-running transactions, then run each statement separately and
outside a transaction before deploying:

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_comment_content_bigm;
DROP INDEX CONCURRENTLY IF EXISTS idx_comment_content_trgm;
```

The subsequent migrations become fast no-ops. If startup performs the drop and
is interrupted instead, `IF EXISTS` makes the next run retry safely; one index
may remain until that retry completes. Dropping the large relation can also
produce a short I/O spike as storage is reclaimed.

An application rollback remains functionally correct without either index, but
legacy search can be much slower. Database rollback must rebuild the selected
GIN and is not immediate; verify the restored index is live, ready, and valid
before relying on the old query's performance:

```sql
SELECT indexrelid::regclass AS index_name, indisvalid, indisready, indislive
FROM pg_index
WHERE indexrelid IN (
    to_regclass('idx_comment_content_bigm'),
    to_regclass('idx_comment_content_trgm')
);
```

## Build the issue properties bigram index after installing pg_bigm

Migration 446 builds `idx_issue_properties_bigm`, the index behind the prefilter
that scalar `contains` property filtering puts in front of its per-key ILIKE.
The runner only executes it where the `gin_bigm_ops` operator class is
installed; everywhere else the version is recorded with its SQL skipped, and the
filter keeps working without index acceleration.

That record is permanent, so a database that gains `pg_bigm` later never builds
the index on its own. Check first:

```sql
SELECT indexrelid::regclass AS index_name, indisvalid, indisready, indislive
FROM pg_index
WHERE indexrelid = to_regclass('idx_issue_properties_bigm');
```

If the index is missing, create it out of band. Run each statement separately
and outside a transaction so the concurrent build is valid:

```sql
CREATE EXTENSION IF NOT EXISTS pg_bigm;
DROP INDEX CONCURRENTLY IF EXISTS idx_issue_properties_bigm;
CREATE INDEX CONCURRENTLY idx_issue_properties_bigm
    ON issue USING gin (LOWER(properties::text) gin_bigm_ops);
ANALYZE issue;
```

The `ANALYZE` is not optional and not a formality. Building an expression index
does not collect statistics for its expression, and until they exist the
planner has nothing to judge the index by: it falls back to a pattern-length
heuristic that estimates a single-character needle at 5% of the table and
leaves `contains` on a sequential scan, with the index built, valid and unused.
Migration 447 does this after 446; an out-of-band build has to do it itself.

Keep the expression exactly as written: the predicate is
`LOWER(properties::text) LIKE LOWER(...)`, and an `ILIKE`-shaped or
non-lowered index would never be used (pg_bigm 1.2 on RDS has no ILIKE index
scan — the constraint migration 036 hit).

Confirm the statistics landed:

```sql
SELECT count(*) FROM pg_statistic WHERE starelid = 'idx_issue_properties_bigm'::regclass;
```

## Migration 627 renumbering

Three migrations were merged with prefix 627, which fails
`TestMigrationNumericPrefixesAreUnique` (CHE-1824). Only one keeps 627:

- `627_notification_channel_preference` keeps its number. `628_notification_channel_preference_index`
  builds an index on its table, so it must stay before 628.
- `627_agent_task_queue_context` is now `630_agent_task_queue_context`.
- `627_unpin_archived_chat_sessions` is now `653_unpin_archived_chat_sessions`.

Migration versions are file basenames, not numeric prefixes, and the runner
applies every file whose version is not in `schema_migrations`, in sorted
order, with no check against the highest applied number. A database that
already recorded an old name therefore runs the identical SQL again under the
new name and records the new version. Leave the historical version rows intact.

Reapplication is a no-op for both renamed files:

- the context migration is `ADD COLUMN IF NOT EXISTS` for two nullable columns,
  and its down is `DROP COLUMN IF EXISTS`;
- the unpin migration is one `UPDATE ... SET pinned_at = NULL WHERE status =
  'archived'`. Handlers keep archived rows unpinned, so a rerun changes nothing
  that a normal write would not. Its down is `SELECT 1`.

The SQL bytes are unchanged. No migration-runner name map entry is needed.
Numbers 631 to 649 are claimed or reserved by open pull requests (wakeups,
inbox) and 650 to 652 by the DM reply record, so 653 is the next free number
on 2026-10-16. Production deployment remains gated on Cheese.

## Upstream v0.6.3 workflow migrations renumbering

Upstream v0.6.3 ships nine project-workflow migrations whose prefixes (573 to
579, 581, 582) collide with fork migrations already applied on deployed
databases. The fork files keep their names. The nine upstream files are new to
this fork, so no database has recorded them, and they take the next free
numbers in their original order (CHE-1837):

- `573_issue_workflow` is now `656_issue_workflow`
- `574_issue_workflow_pkey_index` is now `657_issue_workflow_pkey_index`
- `575_issue_workflow_primary_key` is now `658_issue_workflow_primary_key`
- `576_issue_workflow_workspace_name_index` is now `659_issue_workflow_workspace_name_index`
- `577_project_workflow_id` is now `660_project_workflow_id`
- `578_agent_task_workflow_step` is now `661_agent_task_workflow_step`
- `579_wakeup_skip_handed_to_agent` is now `662_wakeup_skip_handed_to_agent`
- `581_issue_workflow_is_default` is now `663_issue_workflow_is_default`
- `582_issue_workflow_default_index` is now `664_issue_workflow_default_index`

The SQL bytes are unchanged. The runner name map entries for the three index
migrations follow the new names. `allowedForkCollisions` gains no entry. A later
upstream sync must delete these nine upstream filenames when it re-adds them,
or the workflow tables would be created twice.

Migration `665_wakeup_handoff_write_tag` is fork-only. It replaces
`capture_issue_wakeup` from `662_wakeup_skip_handed_to_agent` so a write that
hands an issue to an agent no longer drops that agent's issue-event receipts at
capture (CHE-1837). The receipts carry the write's id and the handler consumes
them once the agent's run exists, so a stop between commit and enqueue cannot
lose the change. A later upstream sync keeps `665` after the re-added upstream
function body, or the zero-delivery window returns.

## Agent task queue compaction columns

Migration `655_agent_task_queue_compact` adds `task_kind`, `compact_reason` and
`compact_trigger_tokens` to `agent_task_queue` (CHE-1807). It was `629_` on the
PR branch, which collided with `629_chat_session_title_trgm_index` on `main`.
It never reached `main`, so no ledger row exists under the old name. It does not
depend on `630_agent_task_queue_context`. Numbers 631 to 649 are claimed by open
wakeup and inbox pull requests, 650 to 652 by the DM reply record, 653 by the
unpin migration and 654 by the chat message `sent_via` validation, so 655 is the
next free number on 2026-10-17. Production deployment remains gated on Cheese.
