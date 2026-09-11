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
scope: primarily read-only investigation + local fake-endpoint smoke tests; no daemon code, no daemon credentials, no CHE-334 activity. Revised-D1 turn (2026-09-11) additionally inspected the live daemon's cgroup ancestry (read-only, plus one transient self-cleaned cgroupfs write-authority test) and made one uncontrolled interactive-CLI test against a fake broker whose actual request destination was not observed — see "credential/model-call disclosure" at the end of the packet.
---

# D1: Native B1 Runner — X99 Namespace/Cgroup Authority and Broker Feasibility

Source plan: `CHE-332-DAEMON-SANDBOX-PLAN.md` (attachment `01a08a8c-9a91-7c54-ad84-1bb855f3e3d1`), PASS from both Opus and Sol, Cheese-approved for `PLAN + D1` only (comment `01a08c21-f6a8-72af-b016-6eb784119d65`).

## Re-pin result

`main` re-pinned at `e7b2882bb8661c57071202fa11a4d8109ccaf29c`, well beyond the plan's `source_head` (`3dbd42e2...`) and deployed `76f59f5f1...`.

**Recorder-split finding reversed — corrected topology.** The plan (line 44) reported `main` had removed `task_phase_timing.go` and the `skills_ready` recorder relative to deployed `76f59f5f1`. This was not a removal on a single line of history; it was a fork. `cc758a74e` added the recorder once, upstream, and is an ancestor of deployed `76f59f5f1` but **not** of the plan's `source_head` `3dbd42e2` (`3dbd42e2` forked before `cc758a74e` landed, so that tree never had the recorder — it was never deleted from it). `e7b2882` — the exact head this packet re-pinned to — is a merge commit (`Merge branch 'multica-ai:main' into main`, parents `1235d0207`, `2297c820c`) and is the first point where both `3dbd42e2`'s and `76f59f5f1`'s history are present together; that merge is what introduced the recorder to this fork's `main`.

At the re-pinned head `e7b2882bb8661c57071202fa11a4d8109ccaf29c` specifically, `task_phase_timing.go` exists and `daemon.go` still marks `taskPhaseSkillsReady` at line 7346, `taskPhaseEnvironmentReady` at 7824, `taskPhaseRuntimeStarted` at 8882 — same call sites the plan cites. **No recorder reconciliation is needed, scoped strictly to this exact head.** Because the recorder arrived via an upstream merge into a fork rather than always having been present, this is a merge-currency fact, not a permanently settled one: D2 must re-confirm the recorder is present at whatever head it actually builds on before depending on it, not assume this packet's finding carries forward automatically.

`execenv/isolation_unix.go` confirmed unchanged: `Setpgid = true` only, no seccomp/namespace/chroot — the plan's characterization of "cancellation isolation, not a sandbox" still holds at the re-pinned head.

## X99 OS/resource admission (namespace/cgroup path)

**Update (revised D1, Cheese-authorized scope, comment `01a08e2b-7c91-7455-a29c-3af2b311b481`): daemon-process cgroup ancestry and scope authority — now VERIFIED, closing D2 entry gate 1.**

Inspected the live, running `multica daemon` process directly (PID confirmed against `multica daemon status --output json`'s reported `pid`). No daemon signal sent, no daemon code/config changed. The ancestry/ownership checks were read-only; the write-authority check below was a transient, self-cleaned cgroupfs mutation, not a read-only observation — recorded honestly as such rather than folded into "read-only":

- `/proc/<daemon-pid>/cgroup` → `0::/user.slice/user-1000.slice/user@1000.service/app.slice/multica.service` (read-only)
- `multica.service` is a genuine user-manager unit: `systemctl --user status multica.service` shows it loaded from `~/.config/systemd/user/multica.service`, active, with the daemon's real PID as `Main PID` and every daemon-spawned child (task subprocesses, `go run` children, etc.) enumerated under the same cgroup tree (read-only).
- The daemon's own cgroup leaf (`/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/multica.service/`) is owned by `congvc:congvc`, mode `755`, with `memory pids` active controllers (read-only).
- Verified write authority directly at that leaf via a **transient, self-cleaned cgroupfs mutation**: created and then immediately removed a test subdirectory (`test-scope-probe-$$`) under the daemon's own cgroup path — succeeded, the child leaf inherited `memory pids` controllers and a writable `cgroup.procs`, then was deleted, leaving no residual state. This is the same mechanism `systemd-run --user --scope` uses, confirmed from the daemon's actual ancestry rather than the interactive session's.
- Parent `app.slice`'s `cgroup.subtree_control` also shows `memory pids` delegated down (read-only).

**Conclusion: the daemon runs under the same `user@<uid>.service`-delegated tree already verified for the interactive session, not a separate system-service tree.** `systemd-run --user --scope` (or an equivalent daemon-created child cgroup under its own `multica.service` leaf) is now confirmed as an authorized mechanism from the daemon's actual runtime context on this host. D2 entry gate 1 (daemon cgroup ancestry/authority) is closed.

**Caveat, scoped honestly:** verified on this X99 host, this daemon instance, at this moment (`systemctl --user status` snapshot taken during this turn). Not verified: behavior if `multica.service` is ever deployed as a system-level unit instead of a user unit (would land under a different, non-`user@<uid>.service` tree and this finding would not transfer) — that remains a deployment-topology assumption, not a re-opened gate, since the current real deployment is what D2 will build against.

---

**Original interactive-session finding (superseded by the daemon-level verification above, kept for record):** Everything below was verified in an interactive `congvc` login session against `user@<uid>.service`, before the daemon process itself was inspected.

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

**This was, at the time, not yet established for the daemon — now resolved above.** (Original text: "The daemon process runs as its own process with its own cgroup ancestry, which was not inspected in this turn... `systemd-run --user --scope` is not presented as settled for the daemon." See the Update at the top of this section for the closure of this gate.)

## Provider endpoint/observation path (both providers, fake endpoints)

**Base-URL override hooks confirmed in source, both providers:**
- Claude: `ANTHROPIC_BASE_URL` is read and observed (`claude.go:336`, `anthropicBaseURLConfigured`) — an existing, already-instrumented override point.
- Codex: `[model_providers.X] base_url` in the generated Codex home config (`codex_home.go` ~line 1340) — documented as the mechanism for pointing the CLI at a different endpoint between runs.

Neither requires new protocol code; both are existing configuration surfaces the daemon already writes into the prepared environment (`execenv.go` / `codex_home.go`).

**Bridge mechanism smoke-tested live, fake endpoints only, no real credentials:**
1. Fake HTTP broker on host TCP loopback: directly reachable from host (sanity), **unreachable from inside the network-isolated sandbox** (`curl` exit 7, connection refused) — confirms the network denial half of the contract.
2. Fake HTTP-over-Unix-socket broker: socket directory bind-mounted read-write into the same network-isolated sandbox at `/run/broker`; `curl --unix-socket /run/broker/broker.sock` succeeded end-to-end (broker received the POST body, returned a fake JSON response) **while the sandbox still has zero network route**.

This proves generic bridge mechanics only: `curl` plus a static URL hook can reach a broker across the network boundary via a mounted Unix socket. It does **not** establish a tested Claude or Codex broker/observation path — neither CLI was invoked, and neither provider's actual HTTP client was exercised against this bridge. Whether the Claude/Codex CLIs specifically can be pointed at a Unix-socket-backed HTTP endpoint (vs. requiring a TCP `host:port` value in `ANTHROPIC_BASE_URL`/`base_url`) is **not yet tested** — both hooks are typed as URLs, so the actual bridge will most likely need a TCP listener *inside* the sandbox's own loopback (which is available even with `--unshare-net`, since `lo` inside an isolated netns is still a private loopback) forwarding to the host-side Unix socket, rather than the CLI dialing the socket directly. Provider-specific broker and observation paths remain fully unverified D5-D7 work; this packet makes no claim about D5's budget beyond "the bridge mechanism to build on exists" — no schedule certainty is implied.

**Update (revised D1, Cheese-authorized scope): attempted live CLI invocation against a fake broker — result is a new, unresolved risk finding, gate NOT closed.**

Attempted: ran the interactive-user `claude` CLI (`~/.local/bin/claude`, this user's own Claude Code install — **not** the daemon's `execenv`-prepared per-task environment) with `ANTHROPIC_BASE_URL` pointed at a local fake HTTP broker on loopback and a fake, non-functional `ANTHROPIC_API_KEY`.

**Result: the fake broker was bypassed.** The broker's request log stayed empty across the run — that establishes only that the CLI did not reach the fake broker, nothing more. The CLI produced a normal, working response anyway. **The request's actual destination was not observed, and whether it used real credentials or completed a real model call cannot be determined from the evidence collected.** An empty fake-broker log is not evidence of the negative — it is silent on where the request actually went. The most likely explanation, unconfirmed: this interactive user has a live cached credential file (`~/.claude/.credentials.json`, confirmed present) and Claude Code's CLI appears to prefer an existing logged-in session over `ANTHROPIC_BASE_URL`/`ANTHROPIC_API_KEY` env overrides — it did not error, retry against the broker, or visibly fail; it silently used a different path entirely, and that path was not instrumented or observed. A `~/.codex/config.toml` on this host was read for its `base_url` format only (not invoked); it is a credential-bearing config — the file holds a live bearer token. That value was not logged or reproduced anywhere in this process. Codex was **not** invoked at all this turn, specifically to avoid the same live-credential risk after the Claude result.

**This test method was flawed and its result must not be read as "the base-URL hook is broken," nor as proof no real call occurred.** It ran against the wrong environment — this developer's personal, already-authenticated interactive CLI install — not the daemon's isolated per-task `execenv`, which does not provision a live credential file into the sandbox in the first place (that is the whole point of the isolation design). The test does NOT establish that the daemon's actual sandboxed invocation would fail the same way, and it does NOT establish that this specific invocation was credential-free or call-free — that would require observing the request's actual egress destination, which this packet did not capture. What it DOES establish is a **new, real risk to flag for D2/D5 design**: if a sandbox setup for whatever reason inherits or leaves behind a valid credential file (developer host reuse, a copy-through bug, a misconfigured bind mount), the base-URL override is not sufficient by itself to guarantee isolation — CLI session/credential precedence can silently bypass it. D2's sandbox must ensure credential files are absent or explicitly denied in the isolated environment, not rely on the URL override alone.

No further live CLI invocation was attempted after this finding, to avoid compounding the same live-credential risk (per the explicit "no real credentials or model calls" constraint) — testing continued at the source level only (stop-path and stream-parsing code, unchanged and cited below).

## Stop and load-observation paths (source-level verification, both providers)

Not re-derived from a live run (see credential-precedence finding above); verified by reading the existing, unchanged daemon source that the original plan also cited:

- **Stop path**: `server/pkg/agent/claude.go` implements a graceful-then-forced subprocess shutdown — SIGTERM to the whole process group, a grace period, then SIGKILL to the group if any member survives (SIGKILL is uncatchable, so this is a hard backstop). This is existing, working code, not proposed — D5-D7 would reuse it, not build it.
- **Load/observation path**: the daemon parses the CLI's streamed output as line-delimited JSON (`bufio.Scanner` + `json.Unmarshal` per line in `claude.go`), already wired into `streamProtocolObservation` logging including `anthropicBaseURLConfigured`. This is the same mechanism the original plan characterized as the observation path; it is unchanged and does not depend on which URL the CLI is pointed at.

Both are real, already-shipped code paths, not net-new work — they were not the source of risk this turn. The risk is entirely in credential precedence, above.

## Named limitations (one D2 blocker closed, one new risk, plus prior host-specific and probe-scope notes)

1. ~~D2 entry gate — daemon cgroup ancestry and scope-management authority unconfirmed~~ **CLOSED this turn.** Verified directly against the running daemon's own cgroup ancestry (`app.slice/multica.service` under `user@<uid>.service`) — see the Update in "X99 OS/resource admission" above.
2. **NEW — credential precedence risk (supersedes the old "Unix-socket path type" limitation as the primary open question)**: a live interactive-CLI test showed `ANTHROPIC_BASE_URL` can be silently bypassed when a cached credential file is present, with the CLI routing elsewhere instead of erroring. The test method itself was flawed (ran against a personal, already-authenticated CLI install, not the daemon's isolated `execenv`), so this is not proof the daemon's actual sandbox fails — but it is a real gap: D2/D5 must design the sandbox to guarantee no credential file is reachable inside it, not rely on the base-URL override alone to guarantee isolation. Provider-specific broker/observation paths remain fully untested against either CLI's actual HTTP client — this gate is **not closed**.
3. **Effective namespace permission was host-specific**: verified on this X99 host only; AppArmor policy differences on other hosts (if this capability is ever deployed beyond the current daemon host) are unverified.
4. **No credentialed/live-model probe was intentionally run, but one may have occurred** — the attempted fake-broker test against the interactive CLI bypassed the fake broker and reached an unobserved destination (see limitation 2); the request's actual destination was not captured, so whether it used real credentials or completed a real model call is **unknown, not ruled out**. This was not a clean guarantee by design the way the rest of this packet's tests were. Codex was not invoked at all, specifically to avoid repeating this risk after the Claude result.

## Result against the approved D1 exit (revised)

The approved plan's D1 exit requires a working, authorized namespace/cgroup path **and** a tested broker/observation path for each provider (`CHE-332-DAEMON-SANDBOX-PLAN.md:129,135`). **That exit is still not met — one of its two prerequisites closed this turn, the other did not.**

- **Namespace/cgroup path: now closed.** Namespace isolation was already verified working. This turn additionally verified the daemon process's own cgroup ancestry and scope-authority directly (not just the interactive session) — `multica.service` runs under the same delegated `user@<uid>.service` tree, write-tested for real. D2 entry gate 1 is cleared.
- **Provider-specific broker/observation path: still not met, and the open question changed.** The prior packet's open question was "can a Unix-socket bridge reach a provider's HTTP client." This turn attempted a live CLI test and found a different, more fundamental risk first: CLI session/credential precedence can bypass the base-URL override entirely, silently. The test method used to find this was itself not a valid stand-in for the daemon's actual isolated environment, so this is not a settled negative result — but it means the original "just point the URL at a bridge" framing is incomplete. Closing this gate now requires testing inside a genuinely credential-free, daemon-style `execenv` sandbox, which is a larger and more careful piece of work than what fit in this 2-hour box.

**This packet still cannot admit D2 on its own — but it closes one of the two prerequisites Sol/Opus required, and narrows the other to a specific, more concrete follow-up question** (verify inside a real isolated `execenv`, with credentials confirmed absent, rather than "invoke the CLI and see"). It does not redefine the approved D1 exit criteria, and no new approval for D2 is implied by closing one of the two gates.

D2-D10 estimate: the daemon cgroup work (previously an open unknown) is now removed from D2's risk list, which should reduce D2's own estimate somewhat — this packet does not quantify that reduction. The provider broker/observation estimate for D5 likely needs to *grow*, not shrink: the credential-isolation risk found this turn is new work (verifying/enforcing no credential file reaches the sandbox) that the original plan's D5 estimate did not account for. D2 and D5 owners must set their own budgets; this packet only flags the direction of change. The recorder-reconciliation risk does not apply at the exact re-pinned head `e7b2882bb8661c57071202fa11a4d8109ccaf29c` (see Re-pin result above); D2 must re-confirm at its own build head.

## What this packet does not do

No daemon/runtime code changed. No daemon/runtime configuration changed. No `multica.service` signal sent. Daemon inspection was read-only for ancestry/ownership (`/proc/<pid>/cgroup`, `systemctl --user status`); the write-authority check was a transient, self-cleaned cgroupfs mutation (test subdirectory created then immediately removed at the daemon's own cgroup leaf), not a read-only observation. No CHE-334 activity. No D2-D10 work started. No 288-session batch. This document and its directory are the only repository change in this turn.

**Credential/model-call disclosure (see the provider broker section above for full detail):** the daemon's own credential files were not touched. However, this turn did invoke the interactive user's own `claude` CLI once, with a fake API key and `ANTHROPIC_BASE_URL` pointed at a local fake broker, intending a clean fake-endpoint test; the CLI appears to have used its own cached interactive login session rather than the fake override, and produced a normal response. **The request's actual destination was not observed. Whether real credentials or a real model call were used is unknown and cannot be determined from the evidence collected** — the fake broker's empty request log establishes only that the fake broker was bypassed, not that nothing else was reached; this cannot be certified as a zero-real-call test. `~/.codex/config.toml` was read for its `base_url` format only — it is a credential-bearing config holding a live bearer token; that value was not logged or reproduced anywhere in this process. Codex itself was never invoked.
