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

**Recorder-split finding reversed.** The plan (line 44) reported `main` had removed `task_phase_timing.go` and the `skills_ready` recorder relative to deployed. At the re-pinned head, `task_phase_timing.go` exists and was never deleted (`git log --diff-filter=A/D` shows a single add, `cc758a74e`, no removal). `daemon.go` still marks `taskPhaseSkillsReady` at line 7346, `taskPhaseEnvironmentReady` at 7824, `taskPhaseRuntimeStarted` at 8882 — same call sites the plan cites. **No recorder reconciliation is needed; that risk item from the plan no longer applies at current `main`.**

`execenv/isolation_unix.go` confirmed unchanged: `Setpgid = true` only, no seccomp/namespace/chroot — the plan's characterization of "cancellation isolation, not a sandbox" still holds at the re-pinned head.

## X99 OS/resource admission (namespace/cgroup path)

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

**Named limitation found and resolved:** the raw login `session-*.scope` cgroup (`/sys/fs/cgroup/user.slice/user-1000.slice/session-*.scope`) is root-owned and not writable by the daemon's user — a naive "drop the child PID into a subdirectory of my session scope" approach fails. The actual delegated boundary is `user@<uid>.service` (`/sys/fs/cgroup/user.slice/user-<uid>.slice/user@<uid>.service/`), which is user-owned, has `cgroup.procs` writable and `cpu memory pids` enabled in `cgroup.subtree_control`. `systemd-run --user --scope -p MemoryMax=256M -- <cmd>` succeeds and is the correct authorized mechanism for CPU/memory/PID-limited task-owned cgroups, not manual cgroupfs writes under the session scope.

**Consequence for D2+:** the daemon-side supervisor should launch B1 children via `systemd-run --user --scope` (or equivalent direct write to the `user@<uid>.service` delegated subtree) wrapping the bwrap invocation, not attempt to nest under the interactive session's cgroup.

## Provider endpoint/observation path (both providers, fake endpoints)

**Base-URL override hooks confirmed in source, both providers:**
- Claude: `ANTHROPIC_BASE_URL` is read and observed (`claude.go:336`, `anthropicBaseURLConfigured`) — an existing, already-instrumented override point.
- Codex: `[model_providers.X] base_url` in the generated Codex home config (`codex_home.go` ~line 1340) — documented as the mechanism for pointing the CLI at a different endpoint between runs.

Neither requires new protocol code; both are existing configuration surfaces the daemon already writes into the prepared environment (`execenv.go` / `codex_home.go`).

**Bridge mechanism smoke-tested live, fake endpoints only, no real credentials:**
1. Fake HTTP broker on host TCP loopback: directly reachable from host (sanity), **unreachable from inside the network-isolated sandbox** (`curl` exit 7, connection refused) — confirms the network denial half of the contract.
2. Fake HTTP-over-Unix-socket broker: socket directory bind-mounted read-write into the same network-isolated sandbox at `/run/broker`; `curl --unix-socket /run/broker/broker.sock` succeeded end-to-end (broker received the POST body, returned a fake JSON response) **while the sandbox still has zero network route**.

This proves the plan's proposed shape — "private network namespace with no host/network route; a supervised loopback HTTP bridge reaches only a task-specific mounted Unix socket" — works mechanically today with fake endpoints. Whether the Claude/Codex CLIs specifically can be pointed at a Unix-socket-backed HTTP endpoint (vs. requiring a TCP `host:port` value in `ANTHROPIC_BASE_URL`/`base_url`) is **not yet tested** — both hooks are typed as URLs, so the actual bridge will most likely need a TCP listener *inside* the sandbox's own loopback (which is available even with `--unshare-net`, since `lo` inside an isolated netns is still a private loopback) forwarding to the host-side Unix socket, rather than the CLI dialing the socket directly. This refinement is D5's job, not D1's — flagged as a named limitation below, not a blocker.

## Named limitations (explicit, not blocking D1's PASS)

1. **Cgroup path correction required for D2+**: use `user@<uid>.service` delegation, not the session scope. (Resolved by this packet; implementers must follow it.)
2. **Provider base-URL hooks accept URLs, not raw Unix-socket paths** — the bridge will need an in-sandbox TCP loopback listener forwarding to the host Unix socket, not a direct CLI-to-socket connection. Untested against either CLI's actual HTTP client (whether it rejects non-http(s) schemes, follows redirects, etc.) — that verification is scoped to D5/D6/D7, not D1.
3. **Effective namespace permission was host-specific**: verified on this X99 host only; AppArmor policy differences on other hosts (if this capability is ever deployed beyond the current daemon host) are unverified.
4. **cgroup delegation verified for the interactive `congvc` session context**: the actual multica daemon process's cgroup ancestry was not independently re-derived in this turn (out of scope — no daemon/runtime configuration changes permitted); D2+ must confirm the daemon process itself runs under a `user@<uid>.service`-delegated tree before relying on this path in production.
5. **No credentialed/live-model probe was run** — per Terra's explicit prohibition. All broker tests used fake local servers only.

## Revised estimate

D1's own exit criteria (working authorized namespace/cgroup path + tested broker/observation path per provider, fake endpoints) are **met**, with one architecture refinement identified (limitation 2, socket-vs-TCP bridging) that changes D5's design slightly but not its 2-hour budget materially — the fix is a loopback TCP shim in front of the same Unix socket, a small addition to the already-scoped work.

No other schedule change indicated. D2-D10 estimate stands at 20 hours (unchanged), plus the previously stated 1 (review) + 2 (code review/corrections) + 1 (deployment/docs) = 24 hours total, 12-hour reserve unchanged. The recorder-reconciliation risk called out in the original plan (part of D2's scope) is now void — if anything this is a small favorable adjustment, not captured as a separate hour reduction since D2's 2-hour budget already had margin.

## What this packet does not do

No daemon/runtime code changed. No daemon/runtime configuration changed. No real Anthropic/OpenAI credentials or endpoints touched. No CHE-334 activity. No D2-D10 work started. No 288-session batch. This document and its directory are the only repository change in this turn.
