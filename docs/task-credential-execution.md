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

## Authenticated input staging source

The authenticated daemon helper stages only the claim's generated prompt, agent instructions,
workspace context and embedded or claim-pinned skill text under `WorkDir/multica-input` before grant delivery.
Caller-supplied input manifests refuse. Unavailable or changed skill references refuse; no host cache, repository,
shared authentication/configuration, socket, hard link or runtime directory is imported.
Only explicit in-memory bytes are supported. Input staging is not credential-exclusive task tooling.
The namespace control succeeds before staging; staging succeeds before every grant request.
Anchored `openat2` operations refuse symlinks, non-regular files, multiple links and non-private paths.
Canonical paths, directory/file collisions and finite file/count/aggregate limits are validated.
Same-task preparation checks exact existing bytes rather than overwriting changed input or native state.
Changed/unsafe input refuses before credential delivery. Authorized input updates need a separately
implemented trusted refresh path; no automatic replacement exists.
Owned fixtures cover these refusals and both production adapters' launch/resume visibility.

Opted-in native wrapping reopens and verifies each declared input through the anchored workdir.
The same verified read-only descriptors become deterministic per-file read-only namespace mounts.
Changed bytes, unsafe paths or modes and missing input refuse even on direct boundary wrapping.
Native launch/resume cannot overwrite or unlink these declared files; task work and native home remain writable.
Descriptor pinning survives replacement of the host pathname after verification, without a path-based reread.
Only declared inputs receive these mounts. Namespace probes and input-free wrapping retain their original behavior.
This adds immutable native input visibility, not an authenticated refresh mechanism or complete task tooling.

### Claim-pinned server skill resolution

The prepared helper resolves workspace, builtin and plugin references through the existing authenticated
runtime/task skill-bundle endpoint. It uses the claim's owning-daemon credential, not the general client token.
The request stays on the validated fixed Multica origin, with no inherited proxy, redirect or retry.
The response must contain exactly the requested bundles, in claim order, with matching source, ID,
claim-time manifest hash, file count and recomputed byte size. A current but changed server bundle refuses;
the ordinary unmanaged resolver's refresh/cache behavior does not apply to this route.

Resolution uses only bounded in-memory responses. It imports no daemon skill cache or host files.
The request is at most 1 MiB and the response is at most 8 MiB, with a 25-second HTTP timeout.
Existing canonical-path, file/count/aggregate input limits still apply before staging or grant delivery.
The helper leaves the original claim unchanged and uses the same resolved snapshot for staged and native prompts.
Exact same-task input reuses native state; changed pins or bytes never overwrite the retained state.
Unavailable resolution, hard 429, malformed/trailing/unknown-field JSON and wrong bundles refuse without a grant.

The existing server endpoint still serves unmanaged preparing tasks without changing their authorization.
Running-task resolution additionally requires the owning-daemon token and the exact frozen gateway policy.
The current runtime owner, workspace, task, agent/runtime association, private-agent owner, builtin profile,
finite dispatch timestamp and `task-gateway-v1` capability must agree. PAT/JWT, foreign daemons,
unmanaged running tasks, changed bindings and terminal tasks refuse. No operator or provider call occurs.
This is pinned input source support, not authenticated input refresh, native skill registration or complete tooling.
Database/native/full gateway acceptance remains NOT-RUN. The daemon's early opt-in refusal remains.

## Evidence and explicit limits

The broker treats an upstream HTTP 429 as a terminal task-quota refusal. A transport failure before
response headers, an unsupported protocol upgrade or any other HTTP status of 300 or greater records
an unknown outcome and refuses. Forwarding is serialized until the streamed body reaches EOF and
closes successfully. A body read error, cancellation, premature close or close error records an unknown
outcome before releasing admission. Body errors become the fixed local failure, not raw upstream errors.
Concurrent body close unblocks a reader and releases admission exactly once. After a refusal, queued
and later requests return a fixed local 429 or 503 without reaching the gateway. Successful SSE bodies
remain streamed one validated frame at a time; JSON bodies wait for bounded completion validation.
Only supported provider completion, explicit final usage, EOF and successful close permit another request.
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
`TestCredentialGatewayStreamLifecycle` covers complete streams, refused empty inference, truncation, cancellation,
premature close and unsupported upgrade with owned local HTTP fixtures. Additional race tests cover
concurrent read/close and redacted close failures. Admission remains locked through body close.
`TestCredentialExclusiveStreamStopAtProductionAdapters` exercises the same owned ELF launch/resume
seams for both providers after an upstream body truncates. The unknown stop persists before a queued
request or same-task preparation can pass. Session identity and already observed usage remain.
The isolated path stops after one gateway request and one native launch. Its ordinary negative control
forwards all three requests and accepts forged success; the denial assertion fails as intended.

These checks prove terminal refusal at owned broker/adapter seams only. Installed Claude/OpenAI native
transport and end-to-end billable-attempt acceptance remain NOT-RUN. Byte-level EOF alone does not establish
provider-protocol completion or normalized final usage. The bounded protocol guard below checks supported
local provider fixtures, not installed native transport, durable accounting or gateway/worker settlement.
Unknown usage is not zero.
The daemon still does not advertise `task-gateway-v1` and still refuses opted-in launch before preparation.
Prepared daemon launch/resume, complete authorized task inputs and credential-exclusive tooling remain unfinished.

## Bounded provider-protocol guard

The broker supports HTTP 200 UTF-8 JSON or SSE on Claude `/v1/messages` and OpenAI `/v1/responses`.
Text content and Claude client `tool_use` blocks are supported. Claude thinking/server-tool blocks
and OpenAI tool/refusal/reasoning items or events refuse; this subset is not complete native CLI compatibility. Chat Completions refuses
before forwarding. `/v1/models` and Claude `/v1/messages/count_tokens` have bounded JSON metadata guards,
not inference usage defaults. Other statuses, media types, protocol upgrades and declared/late trailers refuse.

JSON bodies and individual SSE frames are bounded to 1 MiB with at most 32 JSON nesting levels.
Duplicate JSON keys, including escaped/case-folded names, refuse. LF/CRLF and multiline `data` are supported.
SSE `id`/`retry`, `[DONE]`, partial frames and unknown event variants refuse. Invalid/error frames are not
forwarded, even when fragmented. Successful frames preserve exact wire bytes; the whole SSE response is
not buffered. Closing before the validated output is consumed also records an unknown outcome.

Claude requires message identity/model, supported terminal stop reason and explicit input/cache-write/
cache-read/output counts. SSE block lifecycle must close before a single final delta and `message_stop`.
Cumulative delta counts overwrite monotonically; omitted optional counts retain known start values.
Client tool blocks require unique nonempty IDs, nonempty names and object-valued input. A streamed tool
starts with an empty input object and accepts only `input_json_delta` arguments for that block.
The broker accumulates at most 1 MiB across active tool arguments and validates the complete JSON object
at block stop, including the existing depth and duplicate-field limits. Closed argument buffers are discarded.
Empty-input tools are supported. A response with client tool blocks must terminate with `tool_use`;
that stop reason without a tool block refuses. Malformed or incomplete arguments, unknown usage and
unsupported tool variants retain the existing same-task unknown-outcome stop and prevent another attempt.
Owned JSON/SSE fixtures verify wire preservation, usage and refusal. These are not installed native-tool
or gateway/worker acceptance. The primary protocol reference is the Anthropic streaming documentation.
OpenAI requires stable response identity/model, consecutive event sequence, closed supported items and
`response.completed` with completed status and final usage. The supported streamed message has at most one
text part; SHA-256 digests verify text deltas against text/part/item/final snapshots without retaining the
whole response. Unsupported output, changed identity, incomplete status or terminal error refuses.

All required counters are finite nonnegative decimal int64 values. Missing/null counts are unknown, not
zero. Explicit zero counts remain valid. OpenAI total must equal input plus output without overflow;
cache-read plus cache-write must fit input and reasoning must fit output. The private disjoint buckets
are uncached input, cache creation, cache read and inclusive output. Cache and reasoning are not added
again. These values feed observed broker usage snapshots; no platform usage-reporting, durable settlement,
quota refund or reset API is added. Real provider protocol compatibility and full accounting acceptance remain NOT-RUN.

### Observed usage handoff

`Boundary.UsageSnapshot` returns a defensive per-model copy of the four disjoint counts.
Both production adapters attach that snapshot to `Result.GatewayUsage` for opted-in launch/resume.
Unmanaged results remain unchanged. Native-reported `Result.Usage` and session identity remain unchanged;
native-reported counters are not the trusted gateway snapshot and cannot establish settlement.

The broker records an inference only after supported terminal protocol validation, byte EOF, consumed
validated output and successful upstream body close. Repeated close does not count another inference.
Snapshot completeness is false while a request is in flight, after a stopped/ambiguous outcome, or when
no inference has been observed. Explicit zero counts can be complete; absent counts are not zero.
Previously observed counts survive later quota/unknown outcomes. Metadata requests contribute no tokens.
Per-model accumulation checks the combined four buckets for int64 overflow before changing any counter.
Overflow or exceeding 128 distinct models or a 1024-byte model name records unknown outcome before
another request is admitted. Those are source memory/representation limits, not production quota caps.

Snapshots measure only requests observed by the current prepared broker. They are process-local,
not crash-durable task-wide accounting, quota settlement, reconciliation, or a reset/recovery mechanism.
Re-preparation does not reconstruct earlier observations or change the gateway ledger/native state.

### Prepared daemon execution source

The daemon's `prepareTaskGatewayExecution` connects the authenticated claim, OS/input preparation,
trusted grant, bound broker and existing Claude/Codex production adapters in one owned source path.
It refuses custom arguments, environment, MCP/runtime configuration and unsupported execution options.
The resume session must match the authenticated claim; unavailable resume context refuses rather than
selecting a new session. Model, thinking level and service tier come from that claim, not caller options.
The caller still supplies daemon-owned pinned executable/helper paths and the private root.

`Boundary.VerifyInputs` rechecks the staged bytes and anchored private regular files before each native
launch. Verification is read-only: changed, missing, linked, public or otherwise unsafe inputs refuse.
It never recreates missing files or directories, overwrites input, refreshes a manifest or resets state.
Only one run may use a prepared execution at a time. Close cancels and joins its active run before
closing the broker; repeated close cannot launch another native process. Gateway stops prevent another
run or grant. Results retain the adapters' separately observed gateway usage and native session identity.

Owned ELF/local-HTTP fixtures exercise both adapters' prepared launch/resume, input tampering, override
refusal, single-run/close lifecycle and hard-429/no-new-launch/no-new-grant behavior. They are not installed
native CLI compatibility, PostgreSQL transactions, durable accounting or full gateway acceptance.
This helper remains outside `runTask`. Early opt-in refusal and the unadvertised capability remain.
Authenticated input refresh, credential-exclusive tooling, complete native
transport/protocol support and gateway/worker settlement integration remain unfinished.
The daemon's early refusal and unsupported protocol refusals remain. No launch capability is enabled.

`TestCredentialGatewayProtocolCompletion`, fragmentation/usage and malformed-outcome tests exercise
valid terminal JSON/SSE, incomplete clean EOF, missing/invalid counters, unsupported events, changed
identities, ordering, bounds, trailers and split error redaction. Both production adapters' owned ELF
launch/resume fixtures stop after one gateway call and one native launch for incomplete/error outcomes.
Existing native state, session identity and already observed usage remain; retry flags clear.

`TestCredentialExclusiveProductionAdapters` uses only owned copies of the test executable at the real Claude/Codex production adapter seam. Its ordinary negative control reads an owned unlimited sentinel via direct paths, symlinks, inherited environment, an owned peer's environment/descriptor, a shell helper and owned host TCP/pathname/abstract Unix services. The isolated runs deny those routes, exclude another prepared task, reach only the scoped fixture gateway, override forged request credentials/task headers, preserve same-task home state and exercise resume. Override routes and extra descriptors refuse before launch.

`TestCredentialExecutionPreparationAndRefusal` executes the real namespace control, verifies repeated preparation, refuses unsupported identities/providers, source owner rebind, missing/failed facilities, unsafe native-state symlinks and an unbound gateway. Handler/daemon checks verify authoritative identity derivation and early refusal with no preparation/provisioning/native side effects.

These are source/owned-fixture checks, not native provider transport, gateway/worker acceptance, crash-durable quota-ledger retry, deployment or qualification of every host. No user-installed native agent or real provider is invoked. No live user/container, shared mount/credential, daemon, native model/version, managed key, cap, protected service, merge, deployment or release is changed. Host administrators and the trusted daemon are outside the attacker boundary; an actively compromised host can inject secrets and cannot be repaired by this runner. Source publication does not admit the current shared-credential execution path.

Run the focused evidence from `server`:

```sh
go test ./pkg/credentialexec ./pkg/agent ./internal/daemon ./internal/handler -run TestCredential -count=1 -v
```

The existing handler `TestMain` requires its configured test database even for pure helper tests. Do not point broad DB-backed tests at an unrelated or protected database. This source prerequisite does not authorize creating live containers or mutating protected PostgreSQL data to make a test gate green.
