# steel-browser: provisioning runbook

Plan: CHE-4695, W1 revision 3, F0 (slice W3-1, CHE-4730).
This runbook records the steps. It applies nothing.

## What this plugin is

The Steel gateway's identity in Multica. Its install token (`mpi_…`) is the gateway credential, meant for the gateway on Multica's internal listener. The public Plugin API (`/v1`) also accepts `mpi_` tokens, so the listener split is not a token boundary: a stolen token can be replayed there to read workspace metadata. Whether C00 ingress blocks that path is NOT-VERIFIED. Keep the protections below (hash-only storage, secret-store handoff, rotation after any suspected leak). The manifest asks for one scope, `tasks:read`, no hooks, no `net:` scope and no config field. Multica never calls the gateway; the gateway pulls.

## 1. Read `plugins_v1` (read-only)

The flag gates plugin install and management (`server/internal/featureflags/keys.go`). Read the live value without changing it:

```bash
GET <multica-server-url>/api/config   # anonymous; read `.feature_flags.plugins_v1`
```

Observed 2026-11-09 03:17 ICT against the C00 server URL of this run: `"plugins_v1": false`. So the flag is **off**. Re-read it before step 3; the value can change.

## 2. Install (workspace admin, after the flag is on)

A workspace admin does these steps in the Multica UI or Plugin API. An agent does not hold this role.

1. Publish the package from `plugins/steel-browser/` (`POST /api/workspaces/{id}/plugins/packages`, a zip with `multica.plugin.json` and `skills/steel-browser/SKILL.md`).
2. Preview the version, then install it (`POST /plugins/preview`, `POST /plugins`) with granted scope `tasks:read` only.
3. Enable the installation (`POST /plugins/{installationId}/enable`).
4. Rotate to mint the install token (`POST /plugins/{installationId}/token`). The response shows the token once. Multica stores only its hash. A lost token is rotated, not recovered.

## 3. The install token becomes a C00 gateway secret

1. The admin hands the token straight to the secret store. Never paste it into an issue, a comment, a PR, a log or this repository.
2. The token is stored as a gateway secret on C00 (`gateway install token`). Creating that secret is a C00 change: it needs Cheese's approval, per item, and it is part of the W2-13 first deploy.
3. Rotating or revoking (`DELETE /plugins/{installationId}/token`) replaces or drops the stored hash at once. Rotation after any suspected leak.

## 4. If `plugins_v1` is off: enabling it is a C00 change

It is off today (step 1). Turning it on is a configuration change on C00 Multica.

- Needs **Cheese's approval**, per item. Recorded in plan section "Approvals Cheese gives per item", item 2. **Do not apply** it from this run or from this PR.
- Where: the flag file that `MULTICA_FEATURE_FLAGS_FILE` (or the fallback `MULTICA_JEV_FLAGS_FILE`) points at in the C00 compose `.env`. Set `plugins_v1` `default: true`. Edit the file in place; see `deploy/cd/README.md`. The exact C00 path is NOT-VERIFIED here.
- Reload: flags are loaded once at startup (`server/cmd/server/main.go`). The edit takes effect only after a Multica server restart or an approved rollout, which is a further C00 change that needs Cheese's approval.
- Check: `flagfile-check.mjs` admits the file, and `GET /api/config` shows `plugins_v1: true`.
- Rollback: set it back to `false` in the same file, then restart or roll out again so the server reloads it.
- Also read the plan's other C00 gates: no build on C00; every C00 change needs Cheese's per-item approval.

## Do not

- No account login on Steel and no logged-in work on Steel until W2-14 and W2-16 pass. Agents never type credentials.
- Never call Steel `/context`. Never reuse session `51993bcd-968f-4c7c-a833-5d399c8006c6`.
