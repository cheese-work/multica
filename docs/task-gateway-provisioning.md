# Trusted task gateway source seam

This CHE-1260 candidate builds on CHE-1279's credential-exclusive source boundary.
It does not activate quota execution. The daemon still refuses opted-in tasks.
The complete claim/handoff, native transport, task tooling and acceptance outcome remains unfinished.
No managed key, live credential, production cap, runtime or daemon configuration is changed.

## Trusted policy and operator contract

`server/internal/taskgateway.New` accepts an operator credential and explicit trusted policies.
Each policy pins a canonical platform runtime, runtime owner, workspace and execution task.
The policy also pins the gateway owner, a positive finite cap and both provider key IDs.
Neither a prompt, an agent environment variable nor a parseable task ID authorizes provisioning.
Policy maps are copied on input and on authorization readback.
An empty policy list authorizes no task.
The server loads an optional deployment-owned policy document at startup.
No operator credential or provider key enters the claim payload.

## Default-off server configuration and claim admission

`MULTICA_TASK_GATEWAY_CONFIG` names an absolute, regular, non-symlink JSON file.
The file must not be group/world writable and must be at most 1 MiB.
`MULTICA_TASK_GATEWAY_SECRET_KEY` uses the existing base64 32-byte deployment-key format.
Both variables absent means disabled. Partial, unavailable or invalid configuration stops startup.
This candidate does not change either variable on a live server or daemon.

The document contains `base_url`, `policies` and an encrypted `operator_credential` envelope.
The existing credential keyring opens the envelope with workspace `deployment`, record `task-gateway`
and purpose `operator-api`. The envelope key ID is `credential.KeyID` of the deployment key.
Plaintext operator fields, unknown JSON fields, duplicate fields, an empty policy list,
wrong-purpose envelopes and invalid bindings refuse. The policy document is deployment-trusted,
not task input. It is loaded once and cannot be overridden by an agent environment or prompt.
No plaintext operator credential is persisted by this loader.

Both singular and batch claims apply the policy inside the existing delivery transaction.
The policy gate uses the locked runtime owner, the claimed task and the token's authorized workspace.
Configured runtimes require the exact `task-gateway-v1` capability and frozen policy match.
A protected task cannot escape opt-in by moving to an otherwise unmanaged runtime.
Missing bindings, changed owners/workspaces/tasks, unsupported providers and custom profiles refuse.
A refused response clears stale credential bindings and cannot mint a committed task token.
No claim admission calls the gateway operator; provisioning must still follow OS preparation.
Unmanaged claims keep the prior behavior when no policy covers their runtime or task.

The current daemon deliberately does not advertise `task-gateway-v1` over HTTP or WebSocket.
Its opted-in launch refusal remains in place. This is an old-daemon fence, not activation.
The authenticated grant endpoint and preparation helper exist in source. Daemon launch integration remains unfinished.

The authenticated source contract is Sub2API PR 80 at
`5ca44abeccad64205b8845b531f4e1b8387603e1`, on
`c80cc94f68b2dc8e6d1b4ad0ad94bc0c6813eb09`.
The inspected operator tree matches `7189096ee8e3b12664bb51038d81919acbfa98cc`.

The client uses only these operator routes:

1. `GET /api/v1/admin/users/:owner/api-keys?page_size=100`
2. `PUT /api/v1/admin/task-quotas/:task`
3. `GET /api/v1/admin/task-quotas/:task`

Both configured keys must be owned, active and unexpired.
An operator token cannot become a child credential.
Exactly one key-list page is supported; missing or additional pages refuse before provisioning.
Matching same-task provisioning replays the frozen gateway ledger rather than resetting usage or holds.
The returned snapshot must contain the exact task, owner, key set, finite cap and v1 weighted-token policy.
The exact decimal counters must be canonical, consistent and have positive remaining budget.
Missing usage or lifecycle evidence is not zero or false.
Closed or reconciliation-required ledgers refuse.
No cap-change, reset, refund, release, expiry, reconciliation or automatic retry route is called.

Operator HTTP uses HTTPS, except for literal loopback IP origins used by owned fixtures.
Requests do not inherit a proxy, follow redirects or echo response bodies and secrets in errors.
Responses have a 1 MiB limit and a bounded JSON depth.
Duplicate JSON fields, including case-folded and escaped equivalents, refuse.
Grant formatting and ordinary JSON serialization redact the key.
`Grant.Key` is for an explicit trusted handoff only, not general serialization or child configuration.

## Preparation order and retained state

`Provisioner.Prepare` authorizes the binding, prepares the OS-enforced boundary, provisions the grant,
then binds that exact grant to the broker. A preparation error prevents every operator request.
A provisioning or binding error synchronously closes the prepared boundary.
The successful caller owns `Boundary.Close` and must collect broker shutdown on every exit path.
The caller must supply a fixed trusted private root and pinned executable/helper, not task-controlled paths.
Same-task preparation retains native home/session/cache state.
The existing persisted gateway fingerprint refuses a changed same-task origin or credential.
This is not quota-ledger recovery or crash-durable retry persistence.

## Source evidence and outstanding integration

Owned HTTP fixtures cover both provider keys, same-task replay, forged identity, exhausted/unknown state,
malformed envelopes, duplicate fields, redirects, redaction and HTTP refusal without retry.
Owned ELF fixtures use the real boundary for both provider preparation paths, same-task state retention,
closed-broker refusal, failure-before-provisioning and changed-origin refusal.
No installed native agent or real provider is invoked.

Run the focused checks from `server`:

```sh
go test -race ./internal/taskgateway ./pkg/credentialexec -count=1 -v
go vet ./internal/taskgateway
golangci-lint run ./internal/taskgateway/...
```

Owned configuration/admission tests cover encrypted loading, default-off behavior, malformed policy,
old-daemon capabilities, protected-task movement and exact authenticated identity.
The handler's pure claim-policy seam runs without a database using the production handler sources.
That unit check is not a real transactional database claim test.

## Authenticated daemon handoff source

`POST /api/daemon/runtimes/{runtimeId}/tasks/{taskId}/gateway-grant` requires an existing
`mdt_` daemon credential, not a PAT, JWT or native task token in the authorization header.
Its bounded JSON body carries the existing committed `mat_` task token and exact `dispatched_at`.
Protected claim delivery atomically commits a daemon-token hash alongside the task-token hash.
Only the daemon receives `task_gateway_daemon_token`; neither that credential nor the operator
credential is native-child configuration.

The handoff locks runtime, exact task claim, agent and task token in that order before provisioning.
The authenticated daemon/workspace, locked runtime owner, agent binding, task-token purpose/expiry,
frozen policy and capability must agree. Only dispatched or running same-task claims are supported.
Token revocation, reclaim and identity changes cannot race those locked rows during provisioning.
Authorization and expiry are checked again before commit. A failed commit returns no credential.
The trusted operator's effects are not refunded or reset if subsequent delivery fails.
The explicit response is non-cacheable and contains only the task binding, fixed gateway origin
and selected task key. Ordinary `Grant` JSON and formatting still redact the key.

`Client.PrepareTaskGateway` validates the authenticated claim and fixed server origin, prepares the
actual OS boundary, then requests and binds the grant. No preparation error reaches the handoff.
The request uses a dedicated bounded HTTP client with no proxy, redirect, retry or error-body echo.
HTTP 429 and unsupported responses refuse without another request or unlimited fallback.
Malformed, duplicate, oversized, unknown-field and mismatched-binding handoffs refuse.
Failed fetch/bind closes synchronously; successful callers own `Boundary.Close`.
The caller must supply the trusted private root and pinned executable/helper, not task input.
The daemon helper now selects generated prompt, agent instructions, workspace context and embedded
skill text only from the authenticated claim or exact claim-pinned server bundles. It rejects caller-provided
input manifests and unavailable or changed skill references. The OS namespace control and anchored input
staging both precede every grant request. The owning-daemon skill request has no proxy, redirect or retry.
Staging does not copy host files, shared credentials/configuration, caches, sockets or repositories.
Inputs have canonical relative paths, no file/directory collision, at most 128 files, at most 1 MiB
per file and at most 8 MiB total. Same-task exact input is reused without resetting native state;
changed bytes or unsafe/private-path violations refuse before the handoff. No overwrite/reset exists.
This is a claim-pinned input source slice, not complete tooling or a trusted refresh mechanism.
Every opted-in native launch/resume additionally verifies the exact input tree and mounts its root read-only
through an anchored descriptor. Each declared file retains its own verified read-only descriptor mount.
The child cannot overwrite or unlink declared inputs, rename their ancestors or add input entries.
Other task work and native home stay writable, preserving same-task state. Missing, changed, unsafe or
unexpected entries refuse without repair or a provider call, including during same-task preparation.
Owned ELF fixtures exercise both provider adapters' launch/resume and unchanged writable task-state paths.
These fixtures do not qualify installed native transports, authenticated refresh or full gateway acceptance.
The existing skill endpoint preserves unmanaged preparing-task access. It additionally permits running-task
resolution only through the owning-daemon token with the exact frozen gateway policy and capability.
The server rechecks current task/runtime/agent ownership and a finite dispatch timestamp before reading skills.
Unmanaged running tasks, PAT/JWT, foreign daemons, custom profiles and changed identities refuse.
The shared grant authorization gate retains the grant's additional live task-token checks.
This read-only eligibility check makes no operator or provider call and does not provision or reset a ledger.
The trusted owning daemon is outside the attacker boundary; this endpoint does not remotely attest
OS preparation by a compromised daemon.

Owned row fixtures execute the production handler and verify query identity, authorization locks,
provisioning order, rollback and commit refusal. They do not execute PostgreSQL transactions.
Owned HTTP/ELF fixtures verify daemon preparation order, broker binding, same-task state and refusal.
No installed native agent or real provider is invoked.

The new helper is not wired into `runTask`. The current daemon still refuses before preparation
and does not advertise `task-gateway-v1`; this source activates no live policy or task launch.
The remaining integration must wire the prepared broker to daemon launch/resume.
The integration must stage only authorized task input and support credential-exclusive task tooling.
The broker/production-adapter hard-429 stop now passes owned launch/resume fixtures, including an intended-fail
ordinary negative control. The task stop marker refuses another preparation/provisioning attempt without
clearing native state. See `task-credential-execution.md` for the exact terminal-state contract and limits.
Body truncation, cancellation and premature/failed close now stop the broker with an unknown outcome
before admitting another request. Owned ELF fixtures cover both production adapters' launch/resume
seams; byte-level EOF alone is not provider-protocol completion or normalized final usage evidence.
The broker now requires supported terminal Claude/OpenAI JSON/SSE and explicit final usage before
admitting another request. Missing/invalid usage, incomplete clean EOF, malformed/error frames and
trailers record unknown outcome. Bounded text/client-function protocol support and disjoint usage normalization
are source guards, not complete native transport or durable settlement. Unsupported variants refuse.
See `task-credential-execution.md` for exact bounds and supported events. No daemon launch capability
is enabled by this protocol source slice.
The broker now exposes defensive per-model observed usage snapshots after successful response close.
Both production adapters carry those snapshots separately from native-reported counters.
Snapshot completeness is false for in-flight, stopped/ambiguous or unobserved inference; explicit zero
is distinct from unknown. Metadata is not inference. Observations are process-local to one prepared
broker, not task-wide durable settlement or platform usage reporting. See the execution document for
overflow and memory bounds. This usage handoff does not wire `runTask` or enable the launch capability.
Actual supported native transport and end-to-end hard-429/no-new-billable-attempt/no-fallback acceptance remain unfinished.

The recovered `prepareTaskGatewayExecution` source now owns preparation, authenticated grant binding,
the production adapter and its single-active-run/close lifecycle. It accepts only claim-derived model
and resume identity and narrowly supported timing options. Read-only input verification precedes native
execution; tampering or missing input refuses without repair. Owned ELF/HTTP fixtures cover this path,
not installed native transport, PostgreSQL or durable settlement. The helper is not wired into `runTask`,
does not advertise a capability and does not remove the existing early opt-in refusal. See the execution
document for the remaining authorized-input/tooling and compatibility limits.
Until that complete trusted handoff exists, the daemon's current early refusal must remain.
Native transport and full gateway/worker acceptance are NOT-RUN.
Independent different-lab signing and current-pair OCR/OER-1 remain required before this issue becomes review-ready.
CHE-1279's signing does not sign this integration, and CHE-1253's review is not a runner signature.
