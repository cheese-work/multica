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
