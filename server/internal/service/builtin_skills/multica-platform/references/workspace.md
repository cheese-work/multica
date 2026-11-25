# Workspace administration

Commands that act on the workspace itself, your user, files and exports. Most
writes need owner or admin rights. Agents usually should propose these, not run
them.

## Workspaces

```bash
multica workspace list [--full-id]
multica workspace switch <workspace-id|slug|prefix>
multica workspace create --name "Support" --slug support --issue-prefix SUP
multica workspace update [workspace-id|slug|prefix] --name ... --description ... --issue-prefix ...
multica workspace pause [workspace-id|slug|prefix]    # stop claims for every agent
multica workspace resume [workspace-id|slug|prefix]
multica workspace member invite <email> [workspace] [--role member|admin]
```

- A workspace argument is a full UUID, a slug or a short UUID prefix (4 or more
  hex characters) as shown by `workspace list`.
- `switch` sets the profile default. Resolution order: `--workspace-id`, then
  `MULTICA_WORKSPACE_ID`, then the profile default. It changes where later
  commands go, so a stray `switch` makes them write to the wrong workspace.
  Prefer an explicit `--workspace-id` on each command in an agent run.
- `pause`/`resume` are person owner/admin only; agent tokens get 403. Running
  tasks finish and queued work is kept. `resume` on an unpaused target is a no-op.
- `create` makes you owner. The slug is permanent. It does not change the
  default workspace.
- `invite` sends a pending invitation; the invitee must accept. Owners cannot
  be invited.
- `update` changes metadata and the workspace context. Read the context with
  `--context-file` / `--context-stdin` (byte-for-byte), not inline `--context`.
  `--compact-prompt-file <path>` sets the `/compact` summary prompt; `null`
  resets it.
- `update --expected-before-digest <sha256>` is a compare-and-swap on the
  context: the write applies only if the live value still has that digest of
  its raw UTF-8 bytes. It needs `--context-file` or `--context-stdin`, no other
  update flag, and `--vote-issue <issue-id>` carries the vote record for
  agent callers. A 5xx or dropped connection is AMBIGUOUS and is never retried:
  read the context back before you try again.

Workspace context is standing instruction text for every agent. Do not change it
without the approval the workspace requires.

## Background-agent chains

```bash
multica workspace background-agents list [--output json]
multica workspace background-agents set <job> [--agent <id|name|none>] [--fallback <id|name|none>]
multica me background-agents list [--output json]
multica me background-agents set <job> [--agent <id|name|none>] [--fallback <id|name|none>]
```

- Jobs: `dm_title`, `quick_actions`, `compact`, `routing`. `routing` is
  workspace-only; `me` covers the other three.
- An agent is an id or a name in the target workspace. An ambiguous name is
  refused before any write. `none` clears the field.
- Chain order: your own chain wins when it names a primary agent, else the
  workspace chain, else no agent. `list --output json` shows each job's chain
  and its `source` (`user`, `workspace` or `off`).
- Workspace writes need owner or admin and replace the whole value (the command
  reads, edits one job and writes back). Writes are human-only: a task token gets
  403 `background_agents_human_only`, printed with exit code 3.

## Run queue order

```bash
multica workspace queue-order show [workspace] --output json
multica workspace queue-order set [workspace] [--order priority|project_rank] [--aging-minutes 5..720]
```

- `show` prints the effective `queue_order` (`priority` default) and
  `queue_aging_minutes` (30 default).
- `set` changes only the flags you pass. A value outside the allowed set is
  refused with an error that lists the allowed values, before any request.
- Writes need owner or admin; a member gets 403. Each change is audited as kind
  `workspace.queue_order` (actor, old, new, time): read it with
  `multica audit list --kind workspace.queue_order`.
- Roll back with `--order priority`. Claim-path use of the setting arrives in a
  later release; today it is stored and audited.

## Audit trail

```bash
multica audit list [--kind agent.] [--target <agent name or id>] [--actor <id>] [--since 2h|1d] [--until <time>] [--limit N] [--all] --output json|table
```

- Owner or admin only; a task token is refused (403). Newest first.
- `--kind` is a prefix (`agent.` matches `agent.update`). `--since` and `--until`
  take a duration (`2h`, `1d`) or an RFC 3339 time. `--target` resolves an agent
  name; an ambiguous name errors with the candidates and sends nothing.
- One page by default; the table prints a stderr hint when more rows exist and
  `--output json` carries `next_cursor`. `--all` follows the cursor to the end.
- Redacted fields show the stored `"changed"` marker, never the value.

## Capability routing

`multica workspace capability-routing [workspace-id|slug|prefix]` prints the
workspace setting `capability_routing`: per issue type, the author agent and
the ordered reviewer agents. It prints `{}` when unset. Read-only; change it
with `multica config propose workspace.settings --set capability_routing:=<json>`.

## Agent role scope

`multica workspace agent-role-scope [workspace-id|slug|prefix]` prints the
workspace setting `agent_role_scope`: per agent, the work classes another agent
may assign it. An agent with no entry is unrestricted. An agent that assigns an
issue, or requests a review, from an out-of-scope agent is refused with the
`capability_routing` author or reviewers named; a person is not. Read-only;
change it with `multica config propose workspace.settings --set agent_role_scope:=<json>`.
`multica agent get <agent>` shows one agent's scope.

The work class is read from the issue title: `plan`/`design`/`spec`, `decision`,
and `debate` by type; `review(plan|design|spec|decision|debate)` is a plan
review, any other `review` (including `review(security)`) is a code review;
everything else is execution, security-typed implementation work included. A
co-sign is only a child issue titled `security gate: <parent identifier>` under
the work it gates. A path or a `security` type in a title never makes work a
co-sign. To get Sol's co-sign on D8 paths, create that gate issue under the PR's
issue.

## Workspace MCP servers

```bash
multica workspace mcp add <server-name> [workspace] --server-config-file ./server.json
multica workspace mcp update <server-id> [workspace] [--name new] [--server-config-file ./server.json]
multica workspace mcp remove <server-id> [workspace]
```

`add` puts a server in the workspace library and assigns it to no agent; give it
to one with `multica agent mcp add <agent-id> <server-id>`. The payload is one
server entry, the object that sits under a name in `mcpServers`. Use
`--server-config-file` or `--server-config-stdin`: entries carry tokens, and an
inline `--server-config` lands in shell history. `update` renames, replaces the
config, or both; agents keep their assignment across a rename. `remove` also
removes the server from every agent that had it.

## Repository registry

```bash
multica repo list
multica repo add <url>... [--description "..."]   # description only with one URL
multica repo remove <url>...
```

This is the workspace-level registry. For project-specific context use project
resources (`references/projects.md`). `repo checkout` is in
`references/runtimes.md`. Adding an existing URL does not duplicate it.

## User profile

```bash
multica user profile get
multica user profile update --description-file ./profile.md   # --clear to empty it
```

The text is injected into agent briefs as `## Requesting User`. Use
`--description-file` (inside the working directory unless `--allow-external-file`)
or `--description-stdin` to keep multi-line content verbatim.

## Attachments

```bash
multica attachment download <attachment-id> -o ./attachments
```

Save into a directory inside your working directory, never `/tmp` or another
machine-shared path.

## Provenance export

```bash
multica provenance export --workspace <uuid> --issue <id> --thread <comment-uuid> \
  --cutoff 2026-09-01T00:00:00Z [--output table]
```

Read-only. It exports a bounded, redacted package of issues and comment threads
created at or before `--cutoff` and not modified since; a row modified after
the cutoff is excluded, and revisions reported are the export-time values. Only
human workspace owners and admins may run it: agent task tokens are rejected.
Every scope flag is explicit: `--workspace` (exactly one) never falls back to
`--workspace-id`, `MULTICA_WORKSPACE_ID` or the saved config. Each successful
export writes an audit row.

## GitHub stack auto-link mode

```bash
multica github stack-autolink status --repo owner/repo               # read-only; final mode and per-workspace modes
multica github stack-autolink set [--repo owner/repo] --mode off|report|link   # owner or admin only
```

- `status` is open to any member. The final mode is the lowest effective mode
  across every workspace bound to the same GitHub App installation, so read it
  before you expect stack links on a PR.
- `set` without `--repo` changes the workspace default; with `--repo` it
  changes that repo only. Workspace `off` is a shutdown a repo entry cannot
  raise. Run it only when a person asked for it.

## GitHub kill switch

```bash
multica github kill-switch status                      # active halts, read-only
multica github kill-switch on --scope owner/repo       # halt GitHub-triggered dispatch
multica github kill-switch off --scope owner/repo      # resume it
multica github intake list --since <RFC3339> [--limit 50]  # read-only webhook deliveries; no payload or signature
```

- `--scope` is `*` (org-wide, the default), an owner, or `owner/repo`.
- `on` and `off` change dispatch for the whole scope. Run them only when a
  person asked for it.

## Other commands

One line per verb. Run `multica <command> --help` for flags. Read verbs have no side effects.

| Command | What it does |
|---|---|
| `multica attachment history <attachment-id>` | List every revision of an attachment (time, actor, SHA-256) |
| `multica attachment diff <attachment-id> --from N --to M` | Unified diff between two revisions of a text attachment |
| `multica issue description history <id>` | List recorded description revisions |
| `multica issue description diff <id> --from N --to M` | Unified diff between two description revisions |
| `multica issue route <id> --status S [--owner O]` | Move status, optionally hand off, and record reason and confidence in one call (routing steps) |
| `multica issue urgent-orphans` | Open urgent issues with no task and no enabled wakeup |
| `multica issue-status list` | List the workspace status catalog |
| `multica issue-status get <key-or-name>` | Show one status |
| `multica issue-status create --name N --category C` | Add a custom status (write) |
| `multica issue-status archive <key-or-name>` | Archive a status; owner/admin only, refused while a workflow uses it (write) |
| `multica view get <view>` | One saved view's definition (id, id prefix or exact name) |
| `multica wakeup-definition delete <rule>` | Delete a custom wakeup rule or reset one to its inherited settings (write) |
| `multica chat attention` | DMs that wait for you, oldest first |
| `multica chat later <session> [duration\|time]` | Snooze a DM; reuse `--request-id` on retry |
| `multica ci-watch get <owner/name>` | Show a repo's CI watcher settings |
| `multica ci-watch events <owner/name>` | List a repo's recent watcher events |
| `multica ci-watch set <owner/name>` | Change watcher settings; only passed flags are sent (write) |
| `multica decision shadow-report` | Read the System 1 shadow predictor report |
| `multica system1 report` | Read the System 1 qualification report |
| `multica system1 label-queue` | Read the blind label queue |
| `multica system1 label submit` | Write one label for one observation (write) |
| `multica runtime breaker list` | Provider circuit breakers in the workspace |
| `multica runtime breaker reset <provider>` | Close a breaker now; audited (write) |
| `multica git read-token` | Used by the restricted-run `gh` wrapper; the broker holds the token in memory |
| `multica guard check-pr <pr>` | Check a PR against workspace policy and `.github/agent-rules.yml` read from the protected base; edits nothing |
| `multica guard exec <tool> -- <args...>` | Run a tool after the guard has checked, fixed or refused the call |
| `multica guard plugin list` | List the plugins of every scope in the stored policy |
| `multica guard plugin get <name>` | Show every scope's definition of one plugin |
| `multica guard plugin set --file <policy.json>` | Validate a policy and write the action file for `decision request --action-file`; changes nothing until approved |

## CI failure watcher

Workspace admin. Settings are per repo (`<owner/name>`).

- `multica ci-watch get <owner/name>` shows the watcher settings.
- `multica ci-watch set <owner/name> [flags]` changes settings; only the flags
  you pass are sent. Flags: `--mode off|shadow|act`, `--p1-mode` to
  `--p4-mode shadow|act`, `--branches`, `--events`, `--workflows-include`,
  `--workflows-exclude` (comma-separated), `--fix-owner-kind squad|agent` with
  `--fix-owner-id`, `--clear-fix-owner`, `--notify-member <user-id>`,
  `--clear-notify-member`, `--max-reruns`, `--max-new-issues`,
  `--max-owner-wakes`.
- `multica ci-watch events <owner/name> [--limit N]` lists recent watcher
  events (1 to 200, server default 50).

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
