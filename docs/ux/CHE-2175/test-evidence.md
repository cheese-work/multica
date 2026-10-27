# Test-first evidence

## Observed behavioral red

The following tests ran before the relevant implementation changes in this run.
The original base was `080322d5bb55436c0e07b682ac0b9ace59c21a72` on `main`.
These are excerpts from actual runs, not reconstructed failures.

### Open card annotation target

Command:

```sh
pnpm --filter @multica/views exec vitest run chat/components/decision-request-card.test.tsx --maxWorkers=2
```

Observed before implementation, exit 1:

```text
FAIL chat/components/decision-request-card.test.tsx > DecisionRequestCard accessibility > makes only an open ask annotatable, with the agent reply label and marker slot
AssertionError: expected null not to be null
decision-request-card.test.tsx:32:21
Test Files  1 failed (1)
Tests  1 failed | 2 passed (3)
```

The card had no `data-comment-content`. The final wrapper uses the namespaced
source ID `decision-request:<request ID>` to avoid message-ID collisions.

### Explicit API request identity

Command:

```sh
pnpm --filter @multica/core exec vitest run api/client.test.ts -t 'binds a reply to an explicit decision request' --maxWorkers=2
```

Observed before implementation, exit 1:

```text
FAIL api/client.test.ts > ApiClient > chat attachment wiring > sendChatMessage binds a reply to an explicit decision request
- Expected
+ Received
  {
    "content": "> C. CHE-2162

  1",
-   "decision_request_id": "request-2",
  }
Tests  1 failed | 130 skipped (131)
```

### Two open requests

Command (from `server/`, with the managed worktree test database):

```sh
go test ./internal/handler -run '^TestSendChatDecisionReply' -count=1 -v
```

Observed before implementation, exit 1:

```text
TestSendChatDecisionReplyBindsOnlySelectedRequest
reply was not recorded on card 2: no rows in result set
```

The refusal matrix also returned 201 instead of the required error responses.

### Prevent reuse by the remaining request

After the initial implementation, another behavioral regression ran before its
fix. The original reply answered card 2, leaving card 1 as the sole open ask.

```sh
go test ./internal/handler -run '^TestSendChatDecisionReplyBindsOnlySelectedRequest$' -count=1 -v
```

Observed on the intermediate implementation, exit 1:

```text
TestSendChatDecisionReplyBindsOnlySelectedRequest
explicit answer could be guessed onto card 1:
One open decision request in this DM, asked before the message below. A short reply answers it.
--- FAIL: TestSendChatDecisionReplyBindsOnlySelectedRequest
```

The fix binds the source user message to its answer ID. Turn context excludes
that message from remaining-ask inference, and the recording API refuses reuse.

## Observed frontend green

Final focused command, exit 0:

```sh
pnpm --filter @multica/views exec vitest run chat/components/decision-request-card.test.tsx chat/components/use-chat-annotations.test.tsx chat/components/chat-input.test.tsx chat/components/use-chat-controller.test.tsx workflows/workflow-editor-page.test.tsx --maxWorkers=2
```

```text
Test Files  5 passed (5)
Tests  166 passed (166)
```

The full frontend run also observed `api/client.test.ts (131 tests)` passing,
including the explicit-request regression that failed before implementation.

## Observed backend green

The full Go gate observed the complete handler package passing with the race
detector, including all four `TestSendChatDecisionReply` tests:

```text
ok github.com/multica-ai/multica/server/internal/handler 310.165s
```

This includes the same two-request regression that first failed with no answer
on card 2 and later exposed reuse by card 1. The tests now assert both bindings
and the plain-text quote ordering, plus the refusal, hold and rollback cases.

## Full-gate limits

`pnpm lint --concurrency=1` passed: 7 successful tasks, with existing warnings.
`pnpm typecheck --concurrency=1` passed: 10 successful tasks. The Mermaid
sequence used in the PR passed syntax validation and local X99 browser
rendering. GitHub's Mermaid rendering was not observed.

`pnpm test --concurrency=1 -- --maxWorkers=2` failed in the unmodified workflow
editor test `keeps edits made while the save is pending, and saves them against
the response's version`: the `Save` button was not found. The views result was
550 files / 6944 tests passed and 1 file / 1 test failed. Core passed 196 files /
2533 tests; UI passed 2 files / 17 tests; docs passed 4 files / 19 tests.
The workflow file passed in the focused rerun above. The full frontend gate is
still recorded as FAIL, not converted to PASS by the focused rerun.

The full Go command is `make test ENV_FILE=.env.worktree`. Its unmodified
`server/cmd/multica` tests detect the live managed-runtime marker
`workdir/.multica/daemon_task_context.json`. For example:

```text
--- FAIL: TestResolveWorkspaceID_AgentContextSkipsConfig
resolveWorkspaceID() = "", want "config-file-ws" (config fallback)
--- FAIL: TestAgentUpdateNoFieldsErrorPointsAtEnvCommand
agent execution context requires MULTICA_TOKEN to be a task-scoped mat_ token; detected a daemon task marker
FAIL github.com/multica-ai/multica/server/cmd/multica
```

The runtime marker and CLI trust-boundary guards were not modified or bypassed.
No unrelated workflow or CLI source was changed. These full-gate results are
not a waiver and do not authorize ready, merge or deployment.

## Coverage

The final tests assert annotation accessibility, one-card draft isolation,
ordered plain-text quotes, API/controller forwarding, exact-request issue
binding, refusal without writes, the existing hold, single-answer behavior and
transaction rollback. The full-route browser send is not covered by the
component captures; see the UI evidence README.

## Review correction B1 / N2: observed red and green

Parent: `68879a987c23312d1510de10a8f3f68e26a99275`.
The failed/cancelled-turn and `/compact` tests were added before the correction.
The observed behavioral red was at 12:50:28 ICT on October 27, 2026.
The same command passed at 12:59:47 ICT on October 27, 2026.
The initial sandbox-only Go runner failure is not behavioral-red evidence.

Run from `server/`, using this checkout's isolated database:

```sh
set -a; source ../.env.worktree; set +a
go test ./internal/handler -run '^TestSendChatDecisionReply(WithdrawnTurnCanBeRecordedAgain|RefusesCompact)$' -count=1 -v
```

Observed red, exit 1:

```text
TestSendChatDecisionReplyWithdrawnTurnCanBeRecordedAgain/failed
withdrawn answer kept the source message binding
withdrawn answer still suppresses retry context: ... already recorded ...
withdrawn source cannot be recorded again: source message already answers its explicitly selected request
TestSendChatDecisionReplyWithdrawnTurnCanBeRecordedAgain/cancelled
withdrawn answer kept the source message binding
withdrawn answer still suppresses retry context: ... already recorded ...
withdrawn source cannot be recorded again: source message already answers its explicitly selected request
TestSendChatDecisionReplyRefusesCompact: all three subtests
expected 400, got 201
FAIL github.com/multica-ai/multica/server/internal/handler 0.545s
```

Observed green, exit 0, for the same tests and command:

```text
--- PASS: TestSendChatDecisionReplyWithdrawnTurnCanBeRecordedAgain (0.27s)
    --- PASS: TestSendChatDecisionReplyWithdrawnTurnCanBeRecordedAgain/failed (0.16s)
    --- PASS: TestSendChatDecisionReplyWithdrawnTurnCanBeRecordedAgain/cancelled (0.11s)
--- PASS: TestSendChatDecisionReplyRefusesCompact (0.10s)
    --- PASS: TestSendChatDecisionReplyRefusesCompact//compact (0.04s)
    --- PASS: TestSendChatDecisionReplyRefusesCompact/_/compact_keep_decisions_ (0.03s)
    --- PASS: TestSendChatDecisionReplyRefusesCompact//compact_keep_decisions (0.04s)
PASS
ok github.com/multica-ai/multica/server/internal/handler 0.444s
```

### Chosen correction and limits

Option (b) keeps the existing withdrawal rule and worker scheduling semantics.
Withdrawal clears the user-message binding atomically, but keeps the historical
reference. The tests verify the reopened request, disclaimed answer and absent
wake. They also supply a stale message snapshot to context rendering and restore
a stale persisted binding before re-recording. Context and source reuse must
check for a live answer, not just a non-null ID. Re-recording preserves the
source snapshot and re-applies the 10-minute deploy hold.

N2 refuses `/compact`, including whitespace and arguments, without creating a
message, task, answer or issue quote. An additional rollback test injects failure
at the binding-clear query and checks that the answer, request, source binding
and withdrawal receipt all roll back. This rollback test is additional
verification, not part of the observed red claim above.

N1's byte-identical static PNGs and reason are documented in the UI pack README.
N3 and N4 are intentionally skipped: overlay planning order remains unchanged,
and the existing `&nbsp;` multi-note blank-paragraph separator remains unchanged.
Neither optional note changes this correction's scope. Full-route captures and
the fresh independent exact-head review remain with x99-claude-sonnet.

### Correction focused gates

These checks ran again for the correction, not just the previous head:

| Check | Observed result |
| --- | --- |
| `go test -race ./internal/handler -run '^TestSendChatDecisionReply' -count=1 -v` | PASS, all seven named tests, handler 2.491s. Includes withdrawal rollback. |
| Views focused suite, including workflow editor | PASS, 5 files / 166 tests. |
| Core `api/client.test.ts` | PASS, 1 file / 131 tests. |
| `pnpm lint` | PASS, 7 successful tasks, existing warnings only. |
| `pnpm typecheck` | PASS, 10 successful tasks. |
| `bash scripts/test-go.sh --race --only agent` | PASS, `pkg/agent` 97.343s. |
| `go vet -race -tags agentintegration ./...` | PASS, exit 0. No real-agent smoke test was executed. |
| `make sqlc` | PASS, pinned sqlc v1.31.1. |
| `git diff --check` and gofmt check | PASS. |

The CI-equivalent regular backend suite was also invoked. It observed failures
in the unmodified CLI tests that detect the live managed-runtime marker, as
described in the full-gate limits above. The final regular-suite result and CI
snapshot are reported in the PR body and issue return. The runtime marker and
trust-boundary guards remain intact. No check waiver is claimed.

The local `command -v golangci-lint` probe returned exit 1 with no output, so
golangci-lint is NOT-RUN locally. Its CI check remains required.
The full frontend suite is not re-run for this backend-only correction.

## B2: selected request survives withdrawal

The correction parent is `7ef9664dd0a5153094558299ac8da2f93ecd7c67`.
The B1 evidence above is historical. B2 supersedes its binding-clear mechanism,
not the existing worker withdrawal lifecycle or the ability to re-record.

`TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard` was added before the
B2 implementation. Both `failed` and `cancelled` cases use two open requests,
send an explicitly bound reply on card 2, withdraw its answer, and try to
record the same message on card 1 before re-recording on card 2. The test also
checks selected-request context, absence of card-1 writes, and the live binding
after re-recording.

Command from `server/`, using the existing managed worktree database:

```sh
set -a; source ../.env.worktree; set +a
go test ./internal/handler -run '^TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard$' -count=1 -v
```

Observed behavioral red at 14:51:32 ICT on October 27, 2026, exit 1:

```text
withdrawal lost the selected request binding
withdrawn context lost the selected request: ... 2 open decision requests ...
card-2 reply was accepted for card 1: err=<nil> card1_answers=1
--- FAIL: TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard (0.31s)
    --- FAIL: TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard/failed (0.20s)
    --- FAIL: TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard/cancelled (0.10s)
FAIL github.com/multica-ai/multica/server/internal/handler 0.378s
```

An earlier test-build failure and a fixture-only failure are excluded from the
behavioral-red evidence. The excerpt above comes from the subsequent complete
behavioral run, before implementation.

Observed green at 14:58:52 ICT on October 27, 2026, exit 0, for the same test
and command:

```text
--- PASS: TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard (0.40s)
    --- PASS: TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard/failed (0.24s)
    --- PASS: TestSendChatDecisionReplyWithdrawnSelectionStaysOnCard/cancelled (0.16s)
PASS
ok github.com/multica-ai/multica/server/internal/handler 0.727s
```

### Chosen mechanism

B2 option 1 keeps the binding to the withdrawn answer as request provenance.
Context names that selected request even when its answer is withdrawn. The
record guard permits re-recording only on that answer's request. The live
answer replaces the old binding inside the record transaction using a
compare-and-set on the exact message ID. At B2, missing binding rows failed
closed. B3 below supersedes that missing-row behavior.

N5 is addressed by removing the workspace-wide unbind query; no new index or
migration is needed. N6 keeps the binding on human disclaim and agent veto as
well as failed/cancelled turns. Re-recording the same request remains allowed,
matching existing withdrawal semantics, but another request cannot consume
the source. Unbound replies keep their existing behavior.

The earlier withdrawal rollback test now injects failure at request reopening,
not at the removed unbind query. Its answer, request and receipt rollback
assertions remain. N2's `/compact` refusal remains unchanged. No UI source,
dependency, migration, hold policy or worker scheduling behavior changes.

## B3: orphaned bindings do not wedge chat turns

Correction parent: `78ba2e0e14aa9a906a40a2ef3035af856f2bd4b2`.
`TestSendChatDecisionReplyOrphanedBinding` was added before implementation.
Both cases send a bound reply, hard-delete its issue, run
`cleanupDecisionOrphans`, and verify that the request and answer are gone while
the message still has its stale answer ID.

The `context` case builds decision context with no error and treats the source
as unbound. The `record` case uses `createAskedDecisionRecordTx` to record that
source against a surviving request. It asserts success and exactly one saved
answer, rather than accepting an internal error or merely asserting no 500.

From `server/`, the same command observed red and green:

```sh
set -a; source ../.env.worktree; set +a
bash ../scripts/go-test-with-agent-cli-guard.sh -- \
  go test ./internal/handler -run '^TestSendChatDecisionReplyOrphanedBinding$' -count=1 -v
```

### Observed red

October 27, 2026, 16:34:15 ICT, exit 1, before the implementation changed:

```text
orphaned binding wedges chat decision context: no rows in result set
orphaned binding makes decision recording fail: no rows in result set
--- FAIL: TestSendChatDecisionReplyOrphanedBinding (0.26s)
    --- FAIL: TestSendChatDecisionReplyOrphanedBinding/context (0.19s)
    --- FAIL: TestSendChatDecisionReplyOrphanedBinding/record (0.07s)
FAIL github.com/multica-ai/multica/server/internal/handler 0.527s
```

### Observed green

October 27, 2026, 16:37:59 ICT, exit 0, after the shared loader fix:

```text
--- PASS: TestSendChatDecisionReplyOrphanedBinding (0.23s)
    --- PASS: TestSendChatDecisionReplyOrphanedBinding/context (0.14s)
    --- PASS: TestSendChatDecisionReplyOrphanedBinding/record (0.09s)
PASS
ok github.com/multica-ai/multica/server/internal/handler 0.313s
```

### Scope

`loadDecisionAnswerBinding` treats only `pgx.ErrNoRows` as no binding.
Other database errors still propagate. Existing live and withdrawn answer
rows still take B2's selected-request path. No worker scheduling, hold policy,
UI source, dependency, query or migration changes are needed for B3.
