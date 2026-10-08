## Status and scope

This is the source prerequisite for CHE-1279, not live quota activation. CHE-1260 owns the trusted provisioning and native transport integration. No key is minted, enabled or provisioned by this source. The current daemon refuses `require_credential_isolation` before preparation because that handoff is not integrated. Ordinary task launches are unchanged.

The supported source lane is unprivileged Linux/amd64 with installed system Bubblewrap, mandatory user/PID/network/IPC/UTS namespaces and `openat2` beneath/no-symlink support. The namespace control actually executes an owned `/bin/true` before returning a prepared boundary. Missing, unsupported or failed controls refuse the opt-in; no shared-user fallback exists. The qualified X99 Codex executor remains `0.155.1` in `workspace-write`. This change does not install, select, upgrade or reconfigure a native runtime.

## Enforced boundary

`server/pkg/credentialexec` snapshots only the selected native ELF executable, the source-built Multica helper, `/bin/sh`, `/bin/true` and their explicitly resolved ELF loader/library files. It never binds a shared home, a runtime installation directory, `/usr`, `/etc`, a repository cache, host `/proc`, host sockets or host network. Scripts, Node wrappers, custom profiles and unsupported library/interpreter paths fail closed.

Bubblewrap mounts this code-only root read-only. The task receives only its own writable home/workdir, fresh `/tmp`, fresh `/dev` and PID-namespace `/proc`. Directory mounts use anchored `openat2` descriptors rather than following native-controlled symlinks. All capabilities are dropped; no daemon descriptors are passed to the native CLI. The namespace covers the native CLI, its helper and every tool descendant. Killing the owned Bubblewrap process tears down the PID namespace; the existing adapter cancellation/ownership controls remain in place.

The final wrapper replaces the inherited/custom environment, including HOME/XDG/auth, cloud credentials, dynamic-loader overrides, proxies and credential-helper settings. Native code sees a non-secret placeholder, not the real gateway key. The only external conduit is a task-owned Unix socket bound at `/run/gateway.sock`. A helper inside the network namespace offers loopback HTTP to the native CLI. The trusted broker outside the namespace forwards only the selected provider's supported inference/model paths to one fixed gateway origin, without inherited proxy settings. It replaces credentials and `X-Multica-Task-Id` on every request. CONNECT, admin paths, arbitrary origins and fallback helpers do not provide an escape.

Claude opted-in launches use `default`, not `bypassPermissions`. Codex opted-in launches retain `workspace-write` and `on-request`; the adapter refuses permission escalations instead of auto-approving them. Existing unmanaged approval behavior is unchanged. Task-native config/session/cache state remains inside its task home. Host-side usage/session/config helpers do not follow native-controlled state paths in the opted-in route; missing usage evidence remains unknown, not zero.

## Trusted integration contract for CHE-1260

1. Select opt-in from authenticated server policy, not prompts, custom environment or arguments. The transactional final claim gate derives `credential_execution_binding` from the claimed task ID, locked authenticated runtime owner and authorized claim workspace. Ownerless or malformed bindings are unavailable. The runtime owner is not the human originator or an agent-supplied task header. Older daemons must not receive enabled quota tasks without a supported fail-closed capability handshake.
2. Call `credentialexec.Prepare` before any gateway provisioning. `Spec.Root` must be one fixed daemon-owned private state root, never an agent-supplied path or an existing unmanaged task home. Use the claim binding, selected provider, pinned native ELF and this source-built Multica helper. Stage only authorized task input into `WorkDir`; never import shared authentication/config, hard-linked credentials, Unix helper sockets, shared session stores or secret-bearing artifacts. No generic host directory/runtime asset allowlist is accepted.
3. Any preparation/control error prevents provisioning and native launch. Use the existing trusted owner-scoped operator contract to validate authorization, finite cap and the task binding. This prerequisite intentionally cannot attest that an arbitrary opaque key is finite or task-bound: only CHE-1260's authenticated provisioning result can supply `GatewayCredential`. Never pass an operator/admin key to `BindGateway`.
4. Call `BindGateway` with exactly that binding, task key and fixed gateway origin. It reruns the namespace control before exposing a broker. The task ID pins owner/workspace/runtime digests; a persisted non-secret gateway fingerprint rejects changed keys/origins, including after same-task re-preparation. Repeated preparation preserves native state. This is not quota-ledger persistence or recovery of unknown spend.
5. Use `agent.Config{RequireCredentialIsolation: true, CredentialBoundary: boundary, TaskID: binding.TaskID, BuiltinRuntime: true, ExecutablePath: pinnedPath}` and `ExecOptions.Cwd = boundary.WorkDir()`. Custom/extra arguments, launch profiles, unmanaged settings, MCP/hook routes, mismatched directories and inherited descriptors refuse before native launch. `Close` ends the broker and collects its service loop; scoped native state is retained for authorized same-task retry. The parent must replace the current daemon refusal only after its complete trusted handoff, supported native transport, task tooling and failure contracts are implemented and tested.

## Evidence and explicit limits

The broker treats an upstream HTTP 429 as a terminal task-quota refusal. A transport failure before
response headers, an unsupported protocol upgrade or any other HTTP status of 300 or greater records
an unknown outcome and refuses. Forwarding is serialized until the streamed body reaches EOF and
closes successfully. A body read error, cancellation, premature close or close error records an unknown
outcome before releasing admission. Body errors become the fixed local failure, not raw upstream errors.
Concurrent body close unblocks a reader and releases admission exactly once. After a refusal, queued
and later requests return a fixed local 429 or 503 without reaching the gateway. Successful bodies
remain streamed rather than buffered. A complete byte stream permits another supported request.
The upstream transport uses no proxy or reused keepalive connection, so it cannot transparently retry
a request on a reused connection. Already admitted streams are not refunded or reset.

The trusted daemon records `gateway-stop` in the task state with exclusive creation, mode 0600 and
file/directory sync. Only `quota` or `unknown` is valid. Malformed or unavailable markers refuse.
Preparation and launch reject a stopped task before another provisioning call or native launch.
Persistence errors stop the current boundary with an unavailable outcome. No automatic removal,
reset, expiry or release exists. Native session/cache/home state remains intact. This local refusal
marker is not a quota ledger, crash-durable failed-outcome retry queue or recovery of unknown spend.

Both production adapters cancel an active opted-in native process when the broker stops. The trusted
stop overrides native success/cancellation text with a fixed failed result and suppresses every native
resume/fresh-session retry flag before Codex retry selection. The result keeps the session ID and any
already observed usage. Missing usage remains unknown, not zero. Unmanaged adapters remain unchanged.

`TestCredentialGatewayStopIsTerminal` uses a real OS boundary and local HTTP fixtures. Twelve concurrent
requests after a 429, 502 or transport disconnect cause exactly one upstream request. Persisted stop
state refuses same-task preparation without clearing native state. Marker unit tests cover malformed,
public, symlink and directory state. `TestCredentialExclusiveQuotaStopAtProductionAdapters` covers both
production adapter launch/resume seams with owned ELF fixtures that try three requests and forged success.
`TestCredentialGatewayStreamLifecycle` covers complete/empty streams, truncation, cancellation,
premature close and unsupported upgrade with owned local HTTP fixtures. Additional race tests cover
concurrent read/close and redacted close failures. Admission remains locked through body close.
`TestCredentialExclusiveStreamStopAtProductionAdapters` exercises the same owned ELF launch/resume
seams for both providers after an upstream body truncates. The unknown stop persists before a queued
request or same-task preparation can pass. Session identity and already observed usage remain.
The isolated path stops after one gateway request and one native launch. Its ordinary negative control
forwards all three requests and accepts forged success; the denial assertion fails as intended.

These checks prove terminal refusal at owned broker/adapter seams only. Installed Claude/OpenAI native
transport and end-to-end billable-attempt acceptance remain NOT-RUN. Byte-level EOF does not establish
provider-protocol completion or normalized final usage. Semantic/trailer/usage validation of a cleanly
closed but incomplete provider stream remains unfinished. Unknown usage is not zero.
The daemon still does not advertise `task-gateway-v1` and still refuses opted-in launch before preparation.
Prepared daemon launch/resume, authorized task inputs and credential-exclusive tooling remain unfinished.

`TestCredentialExclusiveProductionAdapters` uses only owned copies of the test executable at the real Claude/Codex production adapter seam. Its ordinary negative control reads an owned unlimited sentinel via direct paths, symlinks, inherited environment, an owned peer's environment/descriptor, a shell helper and owned host TCP/pathname/abstract Unix services. The isolated runs deny those routes, exclude another prepared task, reach only the scoped fixture gateway, override forged request credentials/task headers, preserve same-task home state and exercise resume. Override routes and extra descriptors refuse before launch.

`TestCredentialExecutionPreparationAndRefusal` executes the real namespace control, verifies repeated preparation, refuses unsupported identities/providers, source owner rebind, missing/failed facilities, unsafe native-state symlinks and an unbound gateway. Handler/daemon checks verify authoritative identity derivation and early refusal with no preparation/provisioning/native side effects.

These are source/owned-fixture checks, not native provider transport, gateway/worker acceptance, crash-durable quota-ledger retry, deployment or qualification of every host. No user-installed native agent or real provider is invoked. No live user/container, shared mount/credential, daemon, native model/version, managed key, cap, protected service, merge, deployment or release is changed. Host administrators and the trusted daemon are outside the attacker boundary; an actively compromised host can inject secrets and cannot be repaired by this runner. Source publication does not admit the current shared-credential execution path.

Run the focused evidence from `server`:

```sh
go test ./pkg/credentialexec ./pkg/agent ./internal/daemon ./internal/handler -run TestCredential -count=1 -v
```

The existing handler `TestMain` requires its configured test database even for pure helper tests. Do not point broad DB-backed tests at an unrelated or protected database. This source prerequisite does not authorize creating live containers or mutating protected PostgreSQL data to make a test gate green.
