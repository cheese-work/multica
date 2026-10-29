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
multica workspace member invite <email> [workspace] [--role member|admin]
```

- A workspace argument is a full UUID, a slug or a short UUID prefix (4 or more
  hex characters) as shown by `workspace list`.
- `switch` sets the profile default. Resolution order: `--workspace-id`, then
  `MULTICA_WORKSPACE_ID`, then the profile default. It changes where later
  commands go, so a stray `switch` makes them write to the wrong workspace.
  Prefer an explicit `--workspace-id` on each command in an agent run.
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

## GitHub kill switch

```bash
multica github kill-switch status                      # active halts, read-only
multica github kill-switch on --scope owner/repo       # halt GitHub-triggered dispatch
multica github kill-switch off --scope owner/repo      # resume it
```

- `--scope` is `*` (org-wide, the default), an owner, or `owner/repo`.
- `on` and `off` change dispatch for the whole scope. Run them only when a
  person asked for it.
