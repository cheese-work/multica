# Workflows

A workflow is a named, ordered set of status columns. A column may hand the
issue to a handler (agent, squad, member, project lead or issue creator) and
may name next, "needs changes" and guarded exit statuses. A project opts in;
what a run sees and how status writes behave under one is in
[projects.md](projects.md#project-workflows). This file is the command surface
for reading and editing the workflow itself.

Reads work for any workspace member. Writes are owner/admin only.

```bash
multica workflow list --output json
multica workflow get <workflow-id> --output json      # steps, instructions, rules
```

`workflow get` prints each step under `steps`: `status_key`, `handler`,
`instructions`, `next_status_key`, `back_status_key` and `edges`
(guarded exits: `[{"to","guard","timeout","on_timeout"}]`; PR guards need a
`timeout`).

## Change one step's instructions

```bash
multica workflow set-instructions <workflow-id> --status in_progress --file ./step.md
```

Reads the workflow, replaces only the `instructions` of the step whose
`status_key` equals `--status`, saves, and prints the server readback. Every
other step is sent back unchanged. It fails without sending anything when
`--status` matches no step or more than one. Prefer this over `workflow update`
for a wording change.

## Replace steps or change run rules

```bash
multica workflow update <workflow-id> --steps-file ./steps.json
multica workflow update <workflow-id> --max-runs-per-issue 30 --include-child-prs=false
```

- `--steps-file` holds the FULL steps array in the shape `workflow get`
  prints. It replaces all steps, so start from a fresh `workflow get` and edit
  that, never from memory.
- `--max-runs-per-issue N` (1 to 1000) caps the runs one issue can use before
  the engine blocks it; `0` restores the default of 15.
- `--include-child-prs=false` stops PR guards from reading the PRs of
  sub-issues.
- A rule flag left out keeps its value; rule flags work with or without
  `--steps-file`.

Always read the printed readback. A rejected steps array changes nothing.

An instruction edit changes what every future run on every issue in the
workflow is told, so it is a standing-instruction change: follow the
workspace's own change process before running it.

## Run budget

```bash
multica issue budget get <issue-id> --output json
multica issue budget raise <issue-id> --runs <total> --output json
```

- `get` shows the runs used in the issue's budget window, the limit in force,
  its state and what a suggested raise would leave.
- `raise` sets this issue's own cap to `--runs` total runs. `--runs` must be
  above the cap now in force (the workflow's, or an earlier raise). Other
  issues on the workflow keep the workflow's cap.
- `raise` is members only; the server rejects an agent. An agent that hit the
  cap reports the blocker and asks a person; it never raises its own budget.
