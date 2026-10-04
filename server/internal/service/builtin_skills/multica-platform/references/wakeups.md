# Issue wakeups

Use `multica issue wakeup` to arrange a future ordinary run, then finish the
current run. A wakeup persists on the issue; it is not a sleeping process.

- `wakeup events` lists supported business facts. These work with plugins disabled.
- `wakeup create <issue> --agent-id <target> --kind event --event task.completed,task.failed,task.cancelled --task-id <run> --instruction-file ./instruction.md` wakes once. Omit `--agent-id` only when acting as the authenticated agent. A specific run must belong to this issue; if already terminal, registration captures its matching state immediately.
- For a continuing subscription use `--mode continuous`. For task events, use `--filter-agent-id` to match that agent's future runs; this does not replay historical runs. For comment/issue/reaction/attachment changes, use `--filter-actor-type member|agent --filter-actor-id <user-or-agent-id>` to match the actual author/editor. Mutation-only `--filter-agent-id` remains a legacy alias for actor=agent; do not combine it with actor flags.
- Conditions check a stored fact and wake the target when it holds. Pass exactly one `--until-*` flag and no `--event`, `--task-id` or actor/agent filter; the rule stays `kind=event`. Conditions are checked about every 30 seconds. Already-true conditions fire on the next check, except `--until-pr checks`, which ignores results finished before registration. They fire once by default; continuous rules fire again only after the predicate becomes false or its facts change.

```bash
multica issue wakeup create <issue> --until-status in_review --instruction-file ./instruction.md
multica issue wakeup create <issue> --until-pr checks --expires-in 2h --on-timeout wake --instruction-file ./instruction.md
multica issue wakeup create <issue> --until-pr merged --instruction-file ./instruction.md
multica issue wakeup create <issue> --until-children-done --instruction-file ./instruction.md
multica issue wakeup create <issue> --until-children-done --stage 2 --instruction-file ./instruction.md
multica issue wakeup create <issue> --until-issue <other-issue-id> --until-issue-state done --instruction-file ./instruction.md
```

- `--until-status KEY` waits for this issue's status. `--until-pr checks` waits for linked PR checks to finish on the current head, passing or failing; `merged` waits for any linked PR to merge.
- `--until-children-done` waits for all sub-issues to close (`done` or `cancelled`). With `--stage N`, it waits for staged sub-issues through N; stage N must exist. A parent with no sub-issues never fires. The parent assignee already receives the stage wake, so do not add the same condition.
- `--until-issue ISSUE` waits for another workspace issue to reach `done` (default), `ended` (`done` or `cancelled`), or `in_review`. Conditions also support `--until-assignee member|agent|squad:ID`, `--until-label LABEL_ID`, and `--until-property PROPERTY_ID=VALUE` (including JSON); see `multica issue wakeup create --help`.
- `wakeup create <issue> --kind at --after 10m --instruction-file ./instruction.md` schedules one run. Alternatively use `--at <RFC3339>`.
- `wakeup create <issue> --kind every --every 1h --instruction-file ./instruction.md` schedules a repeating check. Or use `--kind cron --cron '0 * * * *' --timezone Asia/Shanghai`.
- `--max-fires N` (1–1000) caps repeating rules; once rules reject it and continuous event rules default to 20. The run reaching the cap still starts, then the rule pauses with `paused_reason=max_fires`. Re-enabling clears the pause and restarts the count.
- `--expires-in 72h` or `--expires-at <RFC3339>` bounds a wait. For event rules, `--on-timeout wake` starts one run with a `wakeup.timeout` fact; the default `end` stops quietly. Recurring checks should have an end date.
- `wakeup list <issue>` / `wakeup get <issue> <id>` show the saved configuration, next time and latest run. Only promise that a reminder is arranged after creation succeeds.
- `wakeup runs <issue> <id>` lists the latest ten runs, including their trigger, check-in note and whether each commented.
- `wakeup trigger <issue> <id>` queues one run now. It is refused on a closed issue or while disabled or paused. `wakeup delete <issue> <id>` removes the rule and pending inputs and withdraws unstarted runs.
- `wakeup update <issue> <id>` uses the same flags as create and replaces the whole configuration, explicitly re-enabling it. Supply all intended fields. Old unclaimed work is withdrawn.
- `wakeup disable <issue> <id>` stops future triggers and withdraws unclaimed work. Users can also turn it off in the issue sidebar. Closing/cancelling/completing the issue disables its wakeups; reopening does not restore them.
- `--parent <comment-id>` keeps result delivery in the original thread.
- `wakeup checkin <issue> <wakeup-id> --note "..."` ends an every/cron run that found nothing worth a reply. Only the running task started by that rule may use it; the `[WAKEUP]` block gives the exact command. The note (1–500 characters) appears in run history and the issue timeline, and the run ends without a comment. Otherwise post a comment when something changed or needs attention.
- Members create the same rules in the issue sidebar. A parent's stage wake appears there as a system rule; a member may turn it off for that issue or customize its instruction.

Read current state with issue get, comment list, and run inspection before
judging business completion. Wait for linked pull requests with `--until-pr`
(`checks` or `merged`) instead of polling a timer. There is no separate CI event;
use the existing GitHub tools to inspect check details or logs after waking. A
failed run does not imply its business goal is complete. Automatic retry chains
are not followed by event filters; subscribe to a new run if needed. Once the
goal is met, disable continuous rules. Wakeups follow ordinary comment delivery,
except a scheduled check may end with `wakeup checkin`.

Self-trigger protection excludes the registering run and runs started by the
same rule when their source identity is available. It does not prevent cycles
between different rules. Avoid mutually triggering continuous comment subscriptions;
when waiting for a person's reply, filter that member explicitly.

Event rules also have runaway protection: a rule pauses with `paused_reason=loop`
when triggered a third time in the same chain without a person in between, or
with `paused_reason=rate` after 12 runs in an hour. Other paused rules stay off
until re-enabled. A PR system rule keeps events received during a rate pause
pending; the scheduler resumes them once fewer than 12 runs remain in the
preceding hour, without requiring another PR event.
