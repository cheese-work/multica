# Issue wakeups

Use `multica issue wakeup` to arrange a future ordinary run, then finish the
current run. A wakeup persists on the issue; it is not a sleeping process.

- `wakeup events` lists supported business facts. These work with plugins disabled.
- `wakeup create <issue> --agent-id <target> --kind event --event task.completed,task.failed,task.cancelled --task-id <run> --instruction-file ./instruction.md` wakes once. Omit `--agent-id` only when acting as the authenticated agent. A specific run must belong to this issue; if already terminal, registration captures its matching state immediately.
- For a continuing subscription use `--mode continuous`. For task events, use `--filter-agent-id` to match that agent's future runs; this does not replay historical runs. For comment/issue/reaction/attachment changes, use `--filter-actor-type member|agent --filter-actor-id <user-or-agent-id>` to match the actual author/editor. Mutation-only `--filter-agent-id` remains a legacy alias for actor=agent; do not combine it with actor flags.
- `wakeup create <issue> --kind at --after 10m --instruction-file ./instruction.md` schedules one run. Alternatively use `--at <RFC3339>`.
- `wakeup create <issue> --kind every --every 1h --instruction-file ./instruction.md` schedules a repeating check. Or use `--kind cron --cron '0 * * * *' --timezone Asia/Shanghai`.
- `wakeup list <issue>` / `wakeup get <issue> <id>` show the saved configuration, next time and latest run. Only promise that a reminder is arranged after creation succeeds.
- `wakeup update <issue> <id>` uses the same flags as create and replaces the whole configuration, explicitly re-enabling it. Supply all intended fields. Old unclaimed work is withdrawn.
- `wakeup disable <issue> <id>` stops future triggers and withdraws unclaimed work. Users can also turn it off in the issue sidebar. Closing/cancelling/completing the issue disables its wakeups; reopening does not restore them.
- `--parent <comment-id>` keeps result delivery in the original thread.

Read current state with issue get, comment list, and run inspection before
judging business completion. For CI, use the existing GitHub tools from a time
wakeup; CI events are not supported here yet. A failed run does not imply its
business goal is complete. Automatic retry chains are not followed by event
filters; subscribe to a new run if needed. Once the goal is met, disable any
continuous configuration. Every wakeup runs under ordinary execution and comment
delivery rules, even when a periodic check finds no change.

Self-trigger protection excludes the registering run and runs started by the
same rule when their source identity is available. It does not prevent cycles
between different rules. Avoid mutually triggering continuous comment subscriptions;
when waiting for a person's reply, filter that member explicitly.
