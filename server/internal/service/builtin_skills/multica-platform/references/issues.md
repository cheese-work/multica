# Issues

Product contracts the runtime brief does not fully encode.

- [PR linking](#pr-linking)
- [`issue get` has no structured ETA](#issue-get-has-no-structured-eta)
- [Custom properties: typed workflow state](#custom-properties-typed-workflow-state)
- [Status changes have server side effects](#status-changes-have-server-side-effects)
- [Relations graph](#relations-graph)
- [Who else is running right now](#who-else-is-running-right-now)
<<<<<<< fc5981740a9c7eea2cc242ea3022d0818c9b15b0
- [Sub-issues: todo starts work now, backlog parks it](#sub-issues-todo-starts-work-now-backlog-parks-it)
- [Per-issue token usage](#per-issue-token-usage)
- [Resolve the thread you answered](#resolve-the-thread-you-answered)
=======
- [Sub-issues and stages](#sub-issues-and-stages)
>>>>>>> 3d3db2fb7eebf2a1397620b3a324d00383ea1dda
- [Charts and files in a comment](#charts-and-files-in-a-comment)
- [Incorrect to correct](#incorrect-to-correct)
- [Comments: update, delete, resolve, search, subscribers](#comments-update-delete-resolve-search-subscribers)

To attach a local file to an existing issue description, use `multica issue update <id> --attachment <local-path>`. The CLI appends the file's Markdown reference to the end of the description; to replace an image, also use `--description-file` to remove the old reference. Do not put local filesystem paths in the description.

## PR linking

A PR is linked to an issue when its **title** or **branch name** contains a
routable issue key (`PREFIX-NUMBER`, e.g. `MUL-123`), or when its title or body
puts the key **right after a closing keyword** (`Closes` / `Fixes` /
`Resolves`, optional `:` then whitespace). A key that appears in the body as a
bare mention links nothing. People can also link a PR by URL or remove one on
the issue page; a removed PR is not linked again by later webhooks.

```text
MUL-123: add the thing the issue asks for     # key in title  → links
agent/dana/mul-123-add-the-thing              # key in branch → links
Closes MUL-123   (body)                       # key after a keyword → links
Related to MUL-123   (body only)              # no link
```

While a PR is open, its automatic links follow the live title, branch, and
body: removing the key drops the link. After merge or close, existing links stay.

### Default for code-changing issue work

When an issue run changes code in a checked-out GitHub repo, the default handoff
is to open or update a PR before posting the final Multica issue comment, unless
the user explicitly asked for a local-only change or no PR. This is a default, not
an unconditional command: if no code changed, say no PR is needed; if PR creation
is blocked by auth, failing tests, or missing remote state, report that blocker
instead of pretending the run is complete.

To make the PR show on the issue, put a routable issue key in the PR **title**
(preferred) or the **branch**. A key that appears only as a bare mention in the
body links nothing.

```text
MUL-123: fix login redirect        # key in title → links
Part of MUL-123                    # body mention only → no link at all
```

In the final issue comment, include the PR URL when a PR exists. If the task did
not produce a PR because no code changed or the user asked not to create one, say
that explicitly.

If the command returns no linked PRs after a PR was opened, check the syntax
first: the key must be in the PR title or branch, or right after a closing
keyword in the body — a bare body mention does not count. When the syntax is the
problem, editing the title re-runs the scan. If a person removed
the PR from the issue, it stays removed until someone links it again.

If the key is already written correctly and the list is still empty, stop editing
the PR blind: another no-op edit cannot fix an integration that never received the
event. Check the integration side instead — whether the app is installed on that
repository, whether the installation is bound to this workspace, whether
auto-linking is turned off for the workspace, and whether the event reached the
platform at all. A delivery that failed is not retried on its own, but it can be
redelivered once the receiving side is fixed. Report what you found in the result
comment rather than repeating the edit.

## Listing and ordering issues

`issue list` reads one page at a time, server maximum 100 issues per `--limit`
(default 50). Advance `--offset` by the number actually returned in that same
response, not by the requested `--limit` — a final partial page returns fewer
rows than asked for. `--output json` wraps the page as `{"issues": [...],
"total": N, "limit": N, "offset": N, "has_more": bool}`; check `has_more`
rather than comparing returned-count to `--limit` to decide whether to fetch
another page. If the server cannot count matching issues, it returns
`failed to count issues` as an error; do not treat that failure as an empty or
complete list. Older servers can substitute the page length for a failed count,
so that value alone is not proof that all matching issues have been read.

`--fields <name,name,...>` (JSON output only) trims each issue to the named
top-level fields. Filtering is client-side after the full response is fetched,
so it shrinks CLI output and agent context cost, not network or server cost.
Use it when scanning many issues for a few fields (e.g. `--fields
id,title,status,priority`). No effect on `--output table`.

`issue reorder` reads the issue's project-scoped status column before writing
its new position; when a legacy total is unavailable or no larger than its
page, it reads through an empty page. A failed request, malformed page, or
duplicate issue stops the operation before any position write — protection
against truncated or repeated pages, not a snapshot across concurrent edits.
There is no CLI bulk-export or `--all` mode.

## `issue get` has no structured ETA

`multica issue get <issue-id>` never returns a structured ETA — the issue model
has only `start_date` and `due_date`, and the CLI does not invent one by
parsing `due_date` or the description. In `--output table` the `ETA` column
always reads `unknown`, distinct from the separate `DUE DATE` column. In JSON
the issue object is unchanged; a sibling `status_read` object is added —
`{"eta": "unknown", "eta_source": "none recorded", "observed_at": <RFC3339>}` —
marking it a client-side projection, not a server-sent `eta`. Do not read
`due_date` as if it were an ETA.

### Saved views

When a request names a saved view ("the Bugs view", a view link), read it
with `view issues` instead of rebuilding its filters from `issue list` flags:
the server applies every condition the view saves — "is not", the advanced
condition tree, relative due dates, project status — which `issue list`
cannot express.

```bash
multica view list --output json
multica view issues "Bugs" --output json --fields identifier,title,status
```

`<view>` is an ID, ID prefix, or exact name. Pages hold at most 100 issues;
while `has_more` is true, pass `next_cursor` to `--cursor`. Views and their
"me" conditions resolve against the runtime owner, so a member's private view
is only visible when that member owns the runtime.

## Custom properties: typed workflow state

Workspaces may define custom issue properties (Severity, Environment, QA
Status, Reviewer, ...). They are the place for durable, typed issue state:
values are validated against the definition (select options, date format,
http(s) URL, member reference, free-form text/URL lists), visible in the
issue sidebar, and addressed by name.

- Read what exists before writing: `multica property list` shows the catalog;
  `multica issue property list <issue-id>` shows values set on the issue.
- Set values by property name and option name — the CLI translates to ids:

```bash
multica issue property set <issue-id> --name Environment --value staging
multica issue property set <issue-id> --name Platforms --value "iOS,Android"
multica issue property set <issue-id> --name Reviewer --value Bohan
multica issue property unset <issue-id> --name Environment
```

- A validation error lists the legal options — fix the value and retry.
- `actor` / `multi_actor` properties (Reviewer, Escalation contact, ...) hold
  workspace members only. `--value` takes a member name, email, UUID, short id,
  or explicit `member:<uuid>`; `multi_actor` takes a comma-separated list
  (duplicates dropped, order kept, max 20).
- `multi_text` / `multi_url` properties hold free-form lists: `--value` is a
  comma-separated list of strings / http(s) URLs (empty entries skipped,
  duplicates dropped, order kept, max 20). They take no options. An entry that
  itself contains a comma cannot survive that form — pass a JSON array instead:
  `--value '["Smith, John","https://en.wikipedia.org/wiki/Washington,_D.C."]'`.
  The array form is always safe; use it whenever any entry has a comma or the
  input starts with `[`, for example `--value '["[draft] spec"]'`. The same
  forms work during creation: `--property 'Aliases=["Smith, John","[draft] spec"]'`.
- Definitions may carry an optional catalog icon; it changes neither the
  property's type nor its value validation.
- Agents cannot create or edit property definitions (owner/admin humans only).
  If a needed property does not exist, propose it in a comment instead.
- Where state belongs: workflow state a human should see and filter by goes in
  a property; the stage the issue is at goes in its status; everything else —
  what you did this run, what you found — goes in the result comment.
- `issue list` filters and sorts by property with the same name addressing:

```bash
multica issue list --property "Impact=High" --property "Impact=Medium" --output json
multica issue list --property "QA Status=__none__" --status in_review --output json
multica issue list --property "Effort>=3" --property "Due<2026-10-01" --output json
multica issue list --sort property:Impact --direction desc --output json
```

- `--property` takes one `Name=Value` per flag. Repeating the same property
  matches ANY of its members; different properties must ALL match. Values are
  option names or ids (select types), `true`/`false` (checkbox), a member
  name/email/id (actor types), or the value itself for text, url, number,
  and date (`YYYY-MM-DD`); for `multi_text` / `multi_url` the value matches
  any single element of the list exactly. The reserved value `__none__` matches
  issues where the property is unset (works for every type; it is not
  index-backed, so use it for targeted audits rather than as a default
  listing filter).
- Comparisons use the same flag: `>`, `>=`, `<`, `<=` on number; `>` (after)
  and `<` (before) on date; `~=` (contains, case-insensitive) on text and
  url. Quote the flag so the shell does not read `<` or `>` as redirection.
  `!=` is rejected (the server has no not-equal filter); `>=` and `<=` on a
  date are rejected with an error naming the strict form to use. Two
  comparisons on one property OR, so they do not form a range. Number and
  date comparisons are not index-backed.
- `--sort property:<name-or-id>` orders select properties by option order —
  an ordinal scale (Low < Medium < High) sorts by meaning — and number/date/
  text/url by value; issues without the property sort last either way.
  Archived properties and types without an order (multi_select, checkbox,
  actor kinds, list types) are rejected up front.
- `issue list` and `issue get` return `properties` as a map of definition id
  to stored value. Add `--resolve-properties` in JSON mode to get the rows
  `issue property list` prints instead (name, type, stored value, display
  names); the CLI makes at most one catalog request for the whole page, so
  no `property list` call is needed:

```bash
multica issue list --status in_progress --output json --resolve-properties
multica issue get <issue-id> --resolve-properties
```

  Read `display` for a single value and `display_values` for a multi-value
  property (multi_select, multi_actor, multi_text, multi_url); `value` keeps
  the stored ids / strings.

## Status changes have server side effects

A status change is not cosmetic — the server enqueues or skips agent work based
on it. These are the contracts, not advice.

The rules below name fixed built-in status keys, not category-wide behaviors.
Custom statuses carry lifecycle semantics only — unstarted, started, done
(successful terminal), closed (cancelled terminal) — and do not inherit Backlog
parking, In Review completion, Blocked failure, or In Progress recovery. Use
the built-in key when its behavior is needed; built-in definitions cannot be
edited or archived.

Archive a custom status only after moving every issue off it, completed and
canceled included. An occupied status returns HTTP 409 with code
`issue_status_in_use` and `issue_count`, and stays active; use Settings > View
issues to move them, then retry. When replacing a terminal status preserve the
lifecycle meaning (`done` to `done`, `closed` to `closed`) — never reopen or
cancel completed work just to retire a status. Archival moves nothing
automatically; historical issues on archived statuses stay readable via an
explicit status filter.

- **`backlog`** parks an agent-assigned issue: the assignee is set but no task
  fires. Moving `backlog → todo` (or any non-done/non-cancelled status) enqueues
  the assigned agent then.
- **`in_progress` / `in_review`** are agent-managed CLI mutations, not automatic
  side effects of a task starting or finishing. The runtime brief asks agents to
  write the state the issue is in whenever their work changes it — not from
  the trigger type or the run's lifecycle, and not gated on being the
  assignee. Writes happen whenever the state changes, mid-turn included: a
  turn that advances the issue's own ask sets `in_progress` as soon as that
  is known, so the board shows the work while it runs; a blocker is recorded
  when it is hit; and the turn must not exit with a stale value — delivered
  the issue's own ask → `in_review`; work continues beyond the turn
  (dispatched sub-issues, partial delivery) → `in_progress`; stuck →
  `blocked`. A turn that produces none of the issue's own deliverable —
  answering a question, consulting on work owned elsewhere — writes nothing
  at any point. The kind of activity never decides this: research, design,
  planning, and review all count as the work exactly when they are what the
  issue asks for (a review-the-PR issue is being worked the moment reviewing
  starts). Questions, discussion, or acknowledgements never move the status.
  Squad leaders: dispatching members is not delivery — a dispatch turn leaves
  the parent `in_progress`, moving to `in_review` only when a later re-trigger
  confirms the overall goal is met.
- **`in_review`** is an accepted issue status. Some workflows use it while a PR
  is open and awaiting review; moving to it is an explicit mutation.
- **`done`** on a child issue can wake its parent's assignee (see Stages in `references/sub-issues.md`).
- **`cancelled`** is a terminal, user-driven decision to close the issue. Like
  `done` it enqueues no new agent work, but it does **not** stop tasks already in
  flight — a run in progress keeps going. To stop a running task, cancel the
  task itself.
  A cancelled issue may also be marked as a **duplicate** of another issue.
  When you cancel an issue because the work already exists elsewhere, mark it
  with `multica issue status <id> cancelled --duplicate-of <original>` rather
  than cancelling and explaining in a comment: only the mark links the two.
  The original must not itself be a duplicate, and an issue that others are
  marked as duplicates of cannot be marked; the command reports both refusals.
  (`GET /api/issues/<id>/duplicates` shows both sides; issue responses carry
  the original as `duplicate_of` with its id, identifier, title and status
  while the mark counts). Moving it to any
  status other than `cancelled` removes the mark, so reopen a duplicate only
  when it is really separate work. Marking logs `duplicate_marked` on the
  duplicate and `duplicate_added` on the original; removing the mark logs
  `duplicate_unmarked` / `duplicate_removed` (`multica issue timeline --action`).
- **Failed issue-triggered tasks** may roll an issue from `in_progress` back to
  `todo` when no active task / retry remains — the main server-owned status
  write on the agent-run path.
- **Project workflows** can limit which statuses an issue takes and turn a
  status change into a handoff to another agent. When the per-turn prompt
  carries a `[Project workflow]` block, read `references/projects.md`.

## Claim ownership without duplicating a run

Assigning an active issue to an agent normally starts a run. When the work is
already underway and the write only records ownership or progress, pass
`--no-start` on every command in that flow:

```bash
multica issue assign <issue-id> --to-id <agent-id> --no-start
multica issue update <issue-id> --assignee-id <agent-id> --no-start
multica issue status <issue-id> in_progress --no-start
```

Before self-assigning, check the target issue's comment history for an existing
claim. The server also suppresses a trusted self-assignment when the exact
`(issue, agent)` pair already has a non-terminal task, but deliberately keeps
same-agent handoffs to a fresh issue starting runs — cross-issue serial chains
and triage batches rely on that.
## Relations graph

`multica issue relations <id> [--depth 1|2]` prints the issue's sidebar graph as
JSON: nodes and typed edges in both directions, including issues that wait on
or mention it. At most 40 nodes; `omitted_count` says how many were left out.

## Who else is running right now

Nothing about concurrent runs is pushed into your prompt: the answer changes
mid-turn and most turns never need it. Ask the server on the turns that do —
before opening a PR against code a sibling issue also touches:

```bash
multica issue runs <issue-id> --active --output json     # in-flight runs on this issue
multica issue runs <issue-id> --siblings --output json   # ...and across the sub-issue family
```

`--active` drops execution history and returns only `queued` / `dispatched` /
`running` / `waiting_local_directory` runs. `--siblings` widens the read to the
issue's family — its parent (or itself, when it has none) plus every child of
that parent — labelling each row with the issue it belongs to. That is how you
find an agent already working a sibling sub-issue before opening a second PR
against the same code. The family read returns a compact row (task, issue,
agent, status, started), not the full execution-log record; for a run's detail
follow the task id with `multica issue run-messages`.

Rows come back running-first, newest-first within a status, and the family read
is capped at 20. When the cap truncates the answer the CLI prints a warning on
stderr — read it. Without that warning a short list means "nobody else is
there"; with it, the list proves nothing about the runs it did not return.

The CLI also prints an `Observed at <RFC3339 timestamp>` note to stderr on every
read: "who's running right now" is a moving target, and that is when your read
happened, not a server-recorded field. `--active` is read-only — always a `GET`,
never a write — so asking never enqueues or affects a run.

Both are advisory reads. Nothing here reserves an issue or serialises anything:
a run you see may finish a second later, and one you don't see may start one.
Coordinate through the issue's comments — the reads tell you whom to coordinate
with.

## Sub-issues and stages

Whether to split an issue at all, `todo` vs `backlog` children, serial chains,
stages and their parent wakeups live in `references/sub-issues.md`. Read it
before creating a sub-issue.

## Per-issue token usage

```bash
multica issue usage <issue-id> --output json
```

Returns aggregated token usage across the issue's runs. In table output `RUNS`
counts terminal runs only — one still in flight is not yet reflected. A total
prefixed with `>=` is a lower bound, not the real total: one or more terminal
runs did not report usage (aborted or infra-failed), so the true figure is at
least that but unknown above it. Do not compare an unprefixed total with a
`>=`-prefixed one without checking which you have.
## Resolve the thread you answered

An answered thread left open keeps reading as pending work. When your reply
settles the thread, post it with `--resolve-thread`:

```bash
multica issue comment add <issue-id> --parent <comment-id> --content-file ./reply.md --resolve-thread
```

- It needs `--parent`: it resolves the thread that reply belongs to.
- Leave it off while the thread still waits on someone: a question to the
  asker, a pending review, follow-up work.
- The comment is posted first. If resolving then fails, the CLI says so; run
  `multica issue comment resolve <comment-id>` and do not repost.

## Charts and files in a comment

Fenced `html` / `mermaid` blocks render in place; `--attachment` adds a file
card. See [charts.md](charts.md).

## Incorrect to correct

PR title (link the issue):

```text
Fix login redirect                  # incorrect — no issue key, won't link
Body-only "Part of MUL-123"         # incorrect — passing mention, won't link
MUL-123: fix login redirect        # correct — links the PR
```

<<<<<<< fc5981740a9c7eea2cc242ea3022d0818c9b15b0
Serial / phased sub-issues (don't start the whole chain at once):

```bash
# incorrect — all fire immediately, no ordering
multica issue create --title "Step 2" --parent <issue-id> --assignee <agent> --status todo
multica issue create --title "Step 3" --parent <issue-id> --assignee <agent> --status todo

# correct — stage them; Stage 1 runs, later stages park and are promoted as
# each stage's barrier closes
multica issue create --title "Step 1" --parent <issue-id> --assignee <agent> --stage 1 --status todo
multica issue create --title "Step 2" --parent <issue-id> --assignee <agent> --stage 2 --status backlog
multica issue create --title "Step 3" --parent <issue-id> --assignee <agent> --stage 3 --status backlog
```

## Comments: update, delete, resolve, search, subscribers
=======
## Issue wakeups

Use `multica issue wakeup` to arrange a future ordinary run, then finish the
current run. A wakeup persists on the issue; it is not a sleeping process.

- `wakeup events` lists supported business facts. These work with plugins disabled.
- `wakeup create <issue> --agent-id <target> --kind event --event task.completed,task.failed,task.cancelled --task-id <run> --instruction-file ./instruction.md` wakes once. Omit `--agent-id` only when acting as the authenticated agent. A specific run must belong to this issue; if already terminal, registration captures its matching state immediately.
- For a continuing subscription use `--mode continuous`. For task events, use `--filter-agent-id` to match that agent's future runs; this does not replay historical runs. For comment/issue/reaction/attachment changes, use `--filter-actor-type member|agent --filter-actor-id <user-or-agent-id>` to match the actual author/editor. Mutation-only `--filter-agent-id` remains a legacy alias for actor=agent; do not combine it with actor flags.
- A condition lets the platform check a stored fact itself and wake the target only when it holds. Pass exactly one `--until-*` flag and no `--event`, `--task-id` or actor/agent filter; the rule stays `kind=event`. The platform checks about every 30 seconds, so a condition that already holds fires on the next check, except `--until-pr checks`, which ignores results that finished before registration. A condition wakes once by default. With `--mode continuous` it fires again only after the predicate turns false or the facts behind it change (a new check result or PR head, another linked PR merging, a resolved conflict coming back, the number of sub-issues changing).
>>>>>>> 3d3db2fb7eebf2a1397620b3a324d00383ea1dda

```bash
multica issue comment update <comment-id> --expected-revision <n> --content-file ./body.md
multica issue comment delete <comment-id>
multica issue comment resolve <comment-id> [--known-as-of <RFC3339Nano>]
multica issue comment unresolve <comment-id>
multica issue search "<query>" [--include-closed] [--limit 20] --output json
multica issue subscriber list <issue-id>
multica issue subscriber add <issue-id> [--user <name> | --user-id <uuid>]
multica issue subscriber remove <issue-id> [--user <name> | --user-id <uuid>]
```

<<<<<<< fc5981740a9c7eea2cc242ea3022d0818c9b15b0
- **Update** needs the comment's current `revision` from `comment list`. If
  another editor got there first the write is rejected: read the latest body
  and merge into it; do not just resend with the newer revision. A changed
  body is a new trigger: runs the old body started that are still in flight
  are cancelled and every agent the new body mentions is queued again.
  Attachments stay as they are.
- **Delete** keeps replies: a comment that has replies stays as an empty
  placeholder so they keep their place.
- **Resolve** folds a whole thread away. Pass `--known-as-of` (the newest
  `created_at` you loaded) so the server rejects the resolve with a
  `thread_changed` conflict if a reply landed since, instead of folding an
  unread reply away. `unresolve` reopens it.
- **Search** matches titles, descriptions and comment bodies, so a conclusion
  that only lives in a thread is findable. `match_source` is a display hint,
  not a filter. A bare number or identifier-shaped query matches by number and
  ranks first, whatever the prefix, so an id pasted from another tracker can
  top the results with an unrelated local issue. Closed issues are excluded
  unless `--include-closed`.
- **Subscribers** default to the caller. `--user` is a fuzzy name match;
  prefer `--user-id`, which cannot match the wrong person.
=======
- `--until-status KEY` — this issue's status is that key (built-in or one of the workspace's statuses).
- `--until-pr checks` — a linked pull request's checks finished, passing or failing, on its current head. `--until-pr merged` — a linked pull request merged. `--until-pr conflicts` — an open linked GitHub pull request has merge conflicts on its current head, for example after another PR merged into its base; with `--mode continuous` it fires again each time a resolved conflict comes back or a new head is still conflicting. With several linked pull requests, any one satisfies it. These conditions inspect only PRs linked directly to this issue; they do not inherit a parent issue's PRs. Checks and conflicts also require server-side GitHub snapshots; conflicts are not detected on Forgejo, Gitea or GitLab pull requests. A missing link or snapshot is recorded in the rule's `last_error` and retried automatically rather than rejecting the rule.
- `--until-children-done` — every sub-issue, staged or not, is closed (`done` or `cancelled`). With `--stage N` it waits only for staged sub-issues up to stage N, and stage N must have at least one. A parent with no sub-issues never fires. The parent's assignee already gets the sub-issue wakeup described under Stages in `references/sub-issues.md`; do not add this condition for the same wake.
- `--until-issue ISSUE` — another issue in this workspace (identifier or UUID) reaches `--until-issue-state`: `done` (default), `ended` (done or cancelled), `in_review`, or `blocked`.
- The same form also accepts `--until-assignee member|agent|squad:ID`, `--until-label LABEL_ID`, and `--until-property PROPERTY_ID=VALUE` (VALUE may be JSON). `multica issue wakeup create --help` lists them.
- `wakeup create <issue> --kind at --after 10m --instruction-file ./instruction.md` schedules one run. Alternatively use `--at <RFC3339>`.
- `wakeup create <issue> --kind every --every 1h --instruction-file ./instruction.md` schedules a repeating check. Or use `--kind cron --cron '0 * * * *' --timezone Asia/Shanghai`.
- `--max-fires N` (1–1000) caps how many runs a repeating rule starts: every, cron, or `--mode continuous`. A once rule rejects it. Continuous event rules, conditions included, default to 20. The run that reaches the cap is still created, then the rule pauses with `paused_reason=max_fires`. Turning the rule back on clears the pause and restarts the count.
- `wakeup checkin <issue> <wakeup-id> --note "..."` ends a scheduled check (every or cron) that found nothing worth a reply. Only the running run that rule started may call it, and that run's `[WAKEUP]` block gives the exact command. The note (1–500 characters) is kept on the run and shows in the rule's run history and the issue timeline. The run then ends without a comment. When something changed, needs attention, or the check is done, post a comment instead.
- `wakeup list <issue>` / `wakeup get <issue> <id>` show the saved configuration, next time and latest run. `wakeup runs <issue> <id>` lists the rule's latest ten runs with their triggers, check-in notes and whether each commented. Only promise that a reminder is arranged after creation succeeds.
- `wakeup trigger <issue> <id>` queues one run now, as if the rule fired. It is refused on a closed issue, or while the rule is turned off or paused for a loop or burst. `wakeup delete <issue> <id>` removes the rule and its pending inputs and withdraws its runs that have not started.
- `wakeup update <issue> <id>` uses the same flags as create and replaces the whole configuration, explicitly re-enabling it. Supply all intended fields. Old unclaimed work is withdrawn.
- `wakeup disable <issue> <id>` stops future triggers and withdraws unclaimed work. Users can also turn it off in the issue sidebar. Closing/cancelling/completing the issue disables its wakeups; reopening does not restore them.
- `--parent <comment-id>` keeps result delivery in the original thread.
- Give waits an end: `--expires-in 72h` (restarts if the rule is re-enabled) or `--expires-at <RFC3339>`. With `--on-timeout wake`, an event rule runs the target once with a `wakeup.timeout` fact when the deadline passes first; the default `end` stops quietly. Recurring checks should carry an end date.
- Members create the same rules from the issue sidebar. The parent's stage wake (see Stages in `references/sub-issues.md`) appears there as a system rule; a member may turn it off for one issue or set its instruction, which your `[WAKEUP]` block then carries.
>>>>>>> 3d3db2fb7eebf2a1397620b3a324d00383ea1dda

## DM progress follows

A follow makes a DM show an issue's progress cards. Members pass
`--chat <chat-session-id>` for a DM they created. Agents pass no `--chat`: the
lane comes from the DM that started the task, and a task with no DM is refused.

- `multica issue follow <issue-id>` starts following.
- `multica issue unfollow <issue-id>` stops following.
- `multica issue mute-follow <issue-id>` mutes the cards, keeping the follow.
- `multica issue unmute-follow <issue-id>` turns the cards back on.
- `multica issue follows --chat <chat-session-id>` lists the follows in a DM.

## Issue wakeups

`multica issue wakeup` persists event/time work. See [wakeups.md](wakeups.md)
for commands, conditions, and lifecycle behavior.
