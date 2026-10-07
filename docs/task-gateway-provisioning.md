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
An empty policy list authorizes no task. This package is not wired into server or daemon configuration yet.

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

The remaining integration must derive opt-in and identity from authenticated server policy,
use existing secret mechanisms and an old-daemon capability fence, and prepare before provisioning.
The integration must stage only authorized task input and support credential-exclusive task tooling.
The native transport and hard-429/no-fallback contract must pass at both production adapter launch/resume seams.
Until that complete trusted handoff exists, the daemon's current early refusal must remain.
Native transport and full gateway/worker acceptance are NOT-RUN.
Independent different-lab signing and current-pair OCR/OER-1 remain required before this issue becomes review-ready.
CHE-1279's signing does not sign this integration, and CHE-1253's review is not a runner signature.
