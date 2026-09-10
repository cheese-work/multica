---
issue: CHE-332
unit: D1
type: feasibility-packet
date: 2026-09-10
author: x99-claude-sonnet-5
repository: cheese-work/multica
re_pinned_head: e7b2882bb8661c57071202fa11a4d8109ccaf29c
plan_source_head: 3dbd42e2fd14c5366cd92c6996c58800ee5dcc59
implementation_authorized: false
scope: read-only investigation + local fake-endpoint smoke tests; no daemon code, no real credentials, no model calls, no CHE-334 activity
---

# D1: Native B1 Runner — X99 Namespace/Cgroup Authority and Broker Feasibility

Source plan: `CHE-332-DAEMON-SANDBOX-PLAN.md` (attachment `01a08a8c-9a91-7c54-ad84-1bb855f3e3d1`), PASS from both Opus and Sol, Cheese-approved for `PLAN + D1` only (comment `01a08c21-f6a8-72af-b016-6eb784119d65`).

## Re-pin result

`main` re-pinned at `e7b2882bb8661c57071202fa11a4d8109ccaf29c`, well beyond the plan's `source_head` (`3dbd42e2...`) and deployed `76f59f5f1...`.

**Recorder-split finding reversed — corrected topology.** The plan (line 44) reported `main` had removed `task_phase_timing.go` and the `skills_ready` recorder relative to deployed `76f59f5f1`. This was not a removal on a single line of history; it was a fork. `cc758a74e` added the recorder once, upstream, and is an ancestor of deployed `76f59f5f1` but **not** of the plan's `source_head` `3dbd42e2` (`3dbd42e2` forked before `cc758a74e` landed, so that tree never had the recorder — it was never deleted from it). `e7b2882` — the exact head this packet re-pinned to — is a merge commit (`Merge branch 'multica-ai:main' into main`, parents `1235d0207`, `2297c820c`) and is the first point where both `3dbd42e2`'s and `76f59f5f1`'s history are present together; that merge is what introduced the recorder to this fork's `main`.

At the re-pinned head `e7b2882bb8661c57071202fa11a4d8109ccaf29c` specifically, `task_phase_timing.go` exists and `daemon.go` still marks `taskPhaseSkillsReady` at line 7346, `taskPhaseEnvironmentReady` at 7824, `taskPhaseRuntimeStarted` at 8882 — same call sites the plan cites. **No recorder reconciliation is needed, scoped strictly to this exact head.** Because the recorder arrived via an upstream merge into a fork rather than always having been present, this is a merge-currency fact, not a permanently settled one: D2 must re-confirm the recorder is present at whatever head it actually builds on before depending on it, not assume this packet's finding carries forward automatically.

`execenv/isolation_unix.go` confirmed unchanged: `Setpgid = true` only, no seccomp/namespace/chroot — the plan's characterization of "cancellation isolation, not a sandbox" still holds at the re-pinned head.

## X99 OS/resource admission (namespace/cgroup path)

**Scope of this section: interactive host / user-manager feasibility only.** Everything below was verified in an interactive `congvc` login session against `user@<uid>.service`. It does not verify the multica daemon process's own cgroup ancestry or its authority to create/manage the proposed scope — that is untested and is called out as an explicit D2 entry gate below, not a settled fact.

**Namespace isolation: WORKING, unprivileged, no daemon/root changes needed.**

```
bwrap --unshare-user --unshare-pid --unshare-net --unshare-uts --unshare-ipc --unshare-cgroup \
  --clearenv --ro-bind /usr /usr --ro-bind /bin /bin --ro-bind /lib /lib --ro-bind-try /lib64 /lib64 \
  --tmpfs /tmp --proc /proc --dev /dev --die-with-parent \
  <cmd>
```

Verified live on this X99 host (bubblewrap 0.9.0, `apparmor_restrict_unprivileged_userns=1`):
- Mandatory (non-`*-try`) user/pid/net/uts/ipc/cgroup namespace unshare succeeds under the current AppArmor policy — the restriction does not block bwrap's own confined profile.
- Network isolation confirmed: `/sys/class/net` absent inside the sandbox, empty `/proc/net/route`. No host/LAN route.
- Filesystem isolation confirmed: a host-only sentinel file outside the bind allowlist is unreachable (`No such file or directory`) from inside the sandbox.

**Named limitation found and resolved, for the interactive session context:** the raw login `session-*.scope` cgroup (`/sys/fs/cgroup/user.slice/user-1000.slice/session-*.scope`) is root-owned and not writable by the user — a naive "drop the child PID into a subdirectory of my session scope" approach fails there. The actual delegated boundary for that interactive user is `user@<uid>.service` (`/sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/`), which is user-owned, has `cgroup.procs` writable and `cpu memory pids` enabled in `cgroup.subtree_control`. `systemd-run --user --scope -p MemoryMax=256M -- <cmd>` succeeds there.

**This is not yet established for the daemon.** The daemon process runs as its own process with its own cgroup ancestry, which was not inspected in this turn (out of scope — no daemon/runtime configuration changes permitted). Whether the daemon runs under a `user@<uid>.service`-delegated tree, a system-service tree, or something else entirely is unknown. `systemd-run --user --scope` is not presented as settled for the daemon.

**D2 entry gate (must pass before D2 proceeds on this path):** confirm the actual multica daemon process's own cgroup ancestry, and confirm it has authority to create/manage a `--user --scope` (or equivalent delegated-subtree) cgroup from wherever it actually runs. If the daemon runs under a system-service tree instead of a user-manager–delegated one, this mechanism does not directly apply and D2 must re-derive the correct delegated boundary for that context before relying on any isolation admission path.

## Provider endpoint/observation path (both providers, fake endpoints)

**Base-URL override hooks confirmed in source, both providers:**
- Claude: `ANTHROPIC_BASE_URL` is read and observed (`claude.go:336`, `anthropicBaseURLConfigured`) — an existing, already-instrumented override point.
- Codex: `[model_providers.X] base_url` in the generated Codex home config (`codex_home.go` ~line 1340) — documented as the mechanism for pointing the CLI at a different endpoint between runs.

Neither requires new protocol code; both are existing configuration surfaces the daemon already writes into the prepared environment (`execenv.go` / `codex_home.go`).

**Bridge mechanism smoke-tested live, fake endpoints only, no real credentials:**
1. Fake HTTP broker on host TCP loopback: directly reachable from host (sanity), **unreachable from inside the network-isolated sandbox** (`curl` exit 7, connection refused) — confirms the network denial half of the contract.
2. Fake HTTP-over-Unix-socket broker: socket directory bind-mounted read-write into the same network-isolated sandbox at `/run/broker`; `curl --unix-socket /run/broker/broker.sock` succeeded end-to-end (broker received the POST body, returned a fake JSON response) **while the sandbox still has zero network route**.

This proves generic bridge mechanics only: `curl` plus a static URL hook can reach a broker across the network boundary via a mounted Unix socket. It does **not** establish a tested Claude or Codex broker/observation path — neither CLI was invoked, and neither provider's actual HTTP client was exercised against this bridge. Whether the Claude/Codex CLIs specifically can be pointed at a Unix-socket-backed HTTP endpoint (vs. requiring a TCP `host:port` value in `ANTHROPIC_BASE_URL`/`base_url`) is **not yet tested** — both hooks are typed as URLs, so the actual bridge will most likely need a TCP listener *inside* the sandbox's own loopback (which is available even with `--unshare-net`, since `lo` inside an isolated netns is still a private loopback) forwarding to the host-side Unix socket, rather than the CLI dialing the socket directly. Provider-specific broker and observation paths remain fully unverified D5-D7 work; this packet makes no claim about D5's budget beyond "the bridge mechanism to build on exists" — no schedule certainty is implied.

## Named limitations (the two D2 blockers, plus host-specific and probe-scope notes)

1. **D2 entry gate — daemon cgroup ancestry and scope-management authority unconfirmed**: cgroup delegation was verified for the interactive `congvc` user-manager session only, not for the daemon process itself. D2 must confirm the daemon's own cgroup ancestry and its authority to create/manage a delegated scope before this path may be relied on for isolation admission. Not resolved by this packet — the interactive finding is a candidate mechanism, not settled guidance.
2. **Provider base-URL hooks accept URLs, not raw Unix-socket paths** — the bridge will need an in-sandbox TCP loopback listener forwarding to the host Unix socket, not a direct CLI-to-socket connection. Untested against either CLI's actual HTTP client (whether it rejects non-http(s) schemes, follows redirects, etc.). The `curl`/static-URL-hook test proved bridge mechanics only; Claude/Codex provider-specific broker and observation paths remain unverified D5-D7 work.
3. **Effective namespace permission was host-specific**: verified on this X99 host only; AppArmor policy differences on other hosts (if this capability is ever deployed beyond the current daemon host) are unverified.
4. **No credentialed/live-model probe was run** — per Terra's explicit prohibition. All broker tests used fake local servers only.

## Result against the approved D1 exit

The approved plan's D1 exit requires a working, authorized namespace/cgroup path **and** a tested broker/observation path for each provider (`CHE-332-DAEMON-SANDBOX-PLAN.md:129,135`). That exit is **not met**.

This packet returns useful feasibility evidence, not a closed exit:

- Namespace isolation (network/filesystem/PID) is verified working, unprivileged, on this host.
- Cgroup delegation is verified working, but only for the interactive `congvc` user-manager session — the daemon's own cgroup ancestry and scope-management authority are unknown (limitation 1, D2 entry gate).
- A generic Unix-socket-to-HTTP bridge is verified working against fake endpoints — but neither the Claude CLI nor the Codex CLI was invoked, so no provider-specific broker or observation path has been tested (limitation 2, unverified D5-D7 work).

Because both prerequisites the approved exit depends on — daemon cgroup authority and provider-specific broker/observation paths — remain untested, **this packet cannot admit D2.** It records the two unresolved prerequisites and the resulting blocker; it does not redefine the approved D1 exit criteria, and no new approval is implied. Closing the exit requires either testing those two items directly, or an explicit decision from Cheese/Terra to accept a narrower exit before D2 proceeds.

D2-D10 estimate stands at 20 hours as previously stated; this packet does not assert that figure is unaffected by the two open prerequisites above — D2 and D5 owners must re-confirm their own budgets once those items are checked. The recorder-reconciliation risk called out in the original plan does not apply at the exact re-pinned head `e7b2882bb8661c57071202fa11a4d8109ccaf29c` (see Re-pin result above), but D2 must re-confirm this at its own build head rather than treat it as permanently closed.

## What this packet does not do

No daemon/runtime code changed. No daemon/runtime configuration changed. No real Anthropic/OpenAI credentials or endpoints touched. No CHE-334 activity. No D2-D10 work started. No 288-session batch. This document and its directory are the only repository change in this turn.
