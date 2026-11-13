# Harness isolation knobs

CHE-3706. Reference for the **Runtime profile** setting (`harness_profile`).
Each agent CLI has its own knobs for where it reads personal config.

## Setting

- Values: `personal` (default, today's behavior), `managed`.
- Resolution: agent `runtime_config.harness_profile` > workspace `settings.harness_profile` > `personal`.
- UI: Settings > Workspace (default), agent inspector, Runtime profile row (override, "Workspace default" clears it).
- `managed + sandbox` (CHE-1067) is reserved. The server rejects `managed_sandbox`.
- Name: the UI says "Runtime profile". The key is `harness_profile` because `runtime_profile` already means a daemon runtime profile.
- Rollback: set the profile back to `personal`. The next task launches as before.

## Knobs per harness

| Harness | Home env | Files seeded | Never copied | Brief channel | Status |
|---|---|---|---|---|---|
| Claude | `CLAUDE_CONFIG_DIR=<env>/claude-home` | `.credentials.json` (0600), `settings.json` (`{"autoMemoryEnabled": false}` plus the allowlisted auth/route `env` keys `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL`, `ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` from the personal one; key names go in the launch receipt, never values), `.claude.json` | `CLAUDE.md`, `settings.json` (hooks), `plugins/`, `skills/`, `agents/`, `commands/` from the personal dir | `--append-system-prompt-file <env>/runtime-brief.md`; no workdir `CLAUDE.md` | built |
| Codex | `CODEX_HOME=<env>/codex-home` (already per task) | `auth.json`, allowlisted `config.toml` (provider, model, auth-store keys only; mode `0600` because providers may hold credentials), assigned skills only | shared `instructions.md`, `config.json`, `skills/`, `plugins/cache` (a link left by an earlier personal run is removed), `$HOME/.agents/skills` (each entry set `enabled=false` in `config.toml`); personal `mcp_servers`, `hooks`, `plugins`, `profiles`, `notify` and instruction-file keys are dropped from `config.toml`; only `windows.sandbox` is kept from `[windows]`, so a native Windows sandbox opt-in is not loosened to danger-full-access | workdir `AGENTS.md` (unchanged) | built |
| Antigravity (agy) | none: no home or MCP env knob; overriding `HOME` also drops provider settings. Observed in code, not verified live | per-task MCP config at `<workdir>/.agents/plugins/multica-mcp/mcp_config.json`; skills at `<workdir>/.agents/skills` | not isolated. the plugin is additive: agy still loads the global `~/.gemini/config/mcp_config.json` servers | none: no system-prompt flag in the launch args (`-p <prompt>` only) | adapter interface only. Launch unchanged; a `managed` agent fails the task (fail-closed). Set the agent to `personal` explicitly |

Assigned skills load as in `personal`: Claude skills land in the workdir `.claude/skills`. MCP config is passed per task. A `managed` task gets only the servers assigned to the agent, and the personal runtime servers (`~/.claude.json`, `~/.codex/config.toml`) are not merged in.

## Launch receipt

Each managed adapter writes `<env>/launch-receipt.json`:

- `provider`, `profile`
- `env_keys`: env keys the adapter set
- `files_seeded`: files placed in the generated home
- `skills`, `mcp_servers`: names loaded
- `excluded_paths`: personal discovery paths not used. Claude and the shared Codex home list only paths that exist; Codex always lists `$HOME/.agents/skills`.

The receipt holds names and paths only, never file contents or credentials.

## Fallbacks and failures

- Managed preparation fails: the task fails. It never falls back to `personal`, so a managed agent never silently runs with personal config.
- Old daemon (no `runtime-profile-v1` capability): the server fails the task with a clear message instead of handing it over. Update the daemon or set the profile to `personal`.
- Managed on a provider without a managed adapter, or an unknown profile value: the daemon fails the task. No silent `personal` launch.
- Managed Claude with no saved MCP config gets an explicit empty set, so `--strict-mcp-config` is always set.

## Known loss: Hindsight hooks

Hindsight hooks come from the personal Claude plugin. A `managed` Claude agent has no plugins, so it loses them until CHE-3699 moves them into the managed home.
