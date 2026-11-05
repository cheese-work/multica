# Harness isolation knobs

CHE-3706. Reference for the **Runtime profile** setting (`harness_profile`).
Each agent CLI has its own knobs for where it reads personal config.

## Setting

- Values: `personal` (default, today's behavior), `managed`.
- Resolution: agent `runtime_config.harness_profile` > workspace `settings.harness_profile` > `personal`.
- UI: Settings > Workspace (default), agent page > Execution (override, "Workspace default" clears it).
- `managed + sandbox` (CHE-1067) is reserved. The server rejects `managed_sandbox`.
- Name: the UI says "Runtime profile". The key is `harness_profile` because `runtime_profile` already means a daemon runtime profile.
- Rollback: set the profile back to `personal`. The next task launches as before.

## Knobs per harness

| Harness | Home env | Files seeded | Never copied | Brief channel | Status |
|---|---|---|---|---|---|
| Claude | `CLAUDE_CONFIG_DIR=<env>/claude-home` | `.credentials.json` (0600), `settings.json` (`{"autoMemoryEnabled": false}`), `.claude.json` | `CLAUDE.md`, `settings.json` (hooks), `plugins/`, `skills/`, `agents/`, `commands/` from the personal dir | `--append-system-prompt-file <env>/runtime-brief.md`; no workdir `CLAUDE.md` | built |
| Codex | `CODEX_HOME=<env>/codex-home` (already per task) | `auth.json`, `config.toml`, assigned skills only | shared `instructions.md`, `skills/`, `plugins/cache`, `$HOME/.agents/skills` (each entry set `enabled=false` in `config.toml`) | workdir `AGENTS.md` (unchanged) | built |
| Antigravity (agy) | none recorded yet | none recorded yet | none recorded yet | none recorded yet | adapter interface only |

Assigned skills and MCP servers load as in `personal`: Claude skills land in the workdir `.claude/skills`, and MCP config is passed per task.

## Launch receipt

Each managed adapter writes `<env>/launch-receipt.json`:

- `provider`, `profile`
- `env_keys`: env keys the adapter set
- `files_seeded`: files placed in the generated home
- `skills`, `mcp_servers`: names loaded
- `excluded_paths`: personal discovery paths that exist and were not used

The receipt holds names and paths only, never file contents or credentials.

## Fallbacks and failures

- Provider without a managed adapter: the daemon logs a warning and launches as `personal`.
- Managed preparation fails: the task fails. It never falls back to `personal`, so a managed agent never silently runs with personal config.

## Known loss: Hindsight hooks

Hindsight hooks come from the personal Claude plugin. A `managed` Claude agent has no plugins, so it loses them until CHE-3699 moves them into the managed home.
