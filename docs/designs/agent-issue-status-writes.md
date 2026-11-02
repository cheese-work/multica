# Decision-bound issue status writes

An agent creates or edits a custom issue status only after a workspace owner
approves that exact change. The mechanism is the CHE-2455 decision-bound write
that `workflow update --decision-id` already uses. This page names the server
routes, the hash, the CLI commands, the tests and the implementation split.

Status: design, CHE-3070. No code changes in this PR.

## Today

- `POST /api/issue-statuses` and `PATCH /api/issue-statuses/{id}` need
  workspace owner or admin (`CreateIssueStatus`, `UpdateIssueStatus` in
  `server/internal/handler/issue_status.go`). The only client is Workspace
  Settings > Issue Statuses. The CLI has no command.
- The route block adds no per-route machine-credential middleware, unlike
  `/api/issue-workflows` (`RequireHumanOrTaskToken`, `RequireHumanActor`); a
  parent group may still add one.
  Child 2 starts with a red test that shows what a task token gets today. If it
  can write, the same change closes that gap.
- A workflow step may create a status, but a machine caller gets
  `errStatusCreateForbidden`. That stays: an agent creates the status first,
  with its own decision, then references it in a workflow.
- Built-in statuses are immutable. A key is immutable. Neither changes here.

## Scope

| In | Out (stay human-only) |
| --- | --- |
| Create a custom status | Archive, reorder |
| Update name, description, category, colour, icon of a custom status | Built-in statuses, key rename |
| | Run-budget and any non-status setting |

## Flow

```mermaid
sequenceDiagram
    participant A as Agent (task token)
    participant R as Different-lab reviewer
    participant H as Cheese (workspace owner)
    participant S as Server
    A->>S: GET /api/issue-statuses (live catalog)
    A->>A: workspace status create|update --print-hash
    A->>R: change + after_sha256
    R-->>A: PASS
    A->>H: decision request, card "Approve write"
    H-->>S: answers "Approve write"
    A->>S: same command with --decision-id
    S->>S: catalog lock, check hashes, verifyDecisionBinding, write, commit
    S-->>A: status row
    A->>S: GET /api/issue-statuses (read back, compare hash)
```

## Governed field

Add `workspace.issue_statuses` to `delegatedWriteFields` in
`workspace_delegated_writes.go`. The workspace setting (`allowed_agents`,
`allowed_fields`, `requires_human_decision`) already gates it. No migration: the
setting is JSON in `workspace.settings`. Default stays "delegates nothing".
The ask heading is `Apply this change to workspace.issue_statuses?`. The ask
text is built by a new `decisiontx.BoundStatusDecision`, which adds two lines
to `BoundWriteDecision` (see "What the card binds").

## Server routes

No new route. The two existing routes learn a bound mode:

| Route | Human | Task token | Other machine credential |
| --- | --- | --- | --- |
| `POST /api/issue-statuses` | owner/admin, unchanged | needs a bound body | 403 |
| `PATCH /api/issue-statuses/{id}` | owner/admin, unchanged | needs a bound body | 403 |
| `PATCH /reorder`, `DELETE /{id}` | owner/admin, unchanged | 403 (`RequireHumanActor`) | 403 |

Router: `RequireHumanOrTaskToken` on `POST /` and `PATCH /{id}`;
`RequireHumanActor` on reorder and delete.

Bound body: the normal create or update fields plus `decision_id`,
`before_sha256`, `after_sha256` (`DecisionBindingFields`). A bound body refuses
`position` and, on create, a missing `key` (400): the server derives a missing
key from the catalog, so the agent could not know `after_sha256`. A machine
caller without `decision_id` gets 403 from `refuseUnboundMachineWrite`.

Handler order, inside one transaction (same order as `UpdateIssueWorkflow`):

1. `refuseUnboundMachineWrite`, then the field checks the human path makes
   (name, description, category, colour, icon, key pattern). Extract these into
   one `validateStatusFields` both paths call.
2. `LockIssueStatusCatalog` (exclusive).
3. Load the whole custom catalog. `CatalogHash(live, 0)` must equal
   `before_sha256`, else 409 (the catalog moved; ask again).
4. Update only: a category change counts the issues on the status
   (`CountIssuesUsingStatusKey`) as `n`. It still needs
   `confirm_category_change` in the body, as for a human. The approval does not
   replace the confirmation. Any other write has `n = 0`.
5. Apply the change in memory. `CatalogHash(result, n)` must equal
   `after_sha256`, else 403 (the agent wrote something other than it hashed, or
   the number of affected issues moved since the ask; the error body carries
   the live `n`).
6. `verifyDecisionBinding(..., "workspace.issue_statuses", before, after)` with
   the optional ask check described under "What the card binds". It inserts the
   single-use `decision_bound_write` row in this transaction.
7. Write the row, commit. A refused or stale write commits nothing, so it
   consumes no decision.
8. Publish `EventIssueStatusChanged` with the agent as actor.
   `publishIssueStatusChanged` takes a `db.Member` today; give it an actor
   type and id.

The `decision_bound_write` row is the audit record. No second audit table.

## Hash definition

Why the whole catalog: a create has no row to hash, and one definition then
covers create and update. It also makes a key collision or a concurrent edit a
409. Cost: any other custom-status write between ask and apply invalidates the
approval. Catalog writes are rare admin actions, so that is acceptable.

Recipe (same style as `workflowStepsHash`), `CatalogHash(catalog, n)`:

1. Take every non-built-in status of the workspace, archived ones included
   (an archived status still owns its key).
2. Project each to
   `{"archived": bool, "category": <wire category>, "color": <lowercase #rrggbb>, "description": ..., "icon": ..., "key": ..., "name": ...}`.
   Leave out `id`, `position`, timestamps and `is_system`.
3. Wrap: `{"recategorized_issues": n, "statuses": [<sorted by key, bytewise>], "workspace_id": "<uuid>"}`.
   `n` is the number of issues whose lifecycle category the write changes: 0
   for every write that does not change a category, and for `before_sha256`.
4. Canonical JSON: UTF-8, keys sorted, no whitespace, no HTML escaping, then
   sha256, lowercase hex.

`before_sha256` hashes the live catalog; `after_sha256` hashes the catalog as
the server will store it, with the impact `n`. The workspace id binds an
approval to one workspace.

Issue assignments change without touching the catalog, so the catalog alone
cannot bind the impact of a category change. `n` in the hash does. The server
reads `n` under the exclusive catalog lock. Assigning a status to an issue takes
the shared side of the same lock, so no assignment can slip in between the count
and the commit. If issues moved onto or off the status after the ask, `n` differs,
`after_sha256` no longer matches and the write is refused with the decision
unused. The agent asks again with the new count.

Put the recipe in `server/internal/issuestatus` (pure, no DB) as
`CatalogHash` and `ApplyChange`. The handler and the CLI both import it, so
there is one definition and no copy like `workflowStepsHash` in
`cmd/multica/governed_decision.go`.

### What the card binds

The hashes are opaque to a person, and the server cannot read a free-text
summary. So the owner is not shown a summary that the server trusts. The card's
decision text carries the change itself, and the server checks it against the
hashes:

```text
Apply this change to workspace.issue_statuses?
before_sha256: <hex>
after_sha256: <hex>
change: {"key":"needs_qa","op":"create","status":{"category":"started","color":"#ff8800","description":"","icon":"","name":"Needs QA"}}
recategorized_issues: 0
```

- `change` is one line of canonical JSON. For create it holds the full status.
  For update it holds `key`, `op` and only the changed fields under `set`.
- `verifyDecisionBinding` gets an optional ask check (nil for existing callers).
  For this field the check requires exactly one `change:` line and exactly one
  `recategorized_issues:` line in the decision text, parses `change`, applies
  `ApplyChange` to the live catalog and requires
  `CatalogHash(result, recategorized_issues)` to equal `after_sha256`. A card
  that shows one change while the hashes cover another is refused, and so is
  a repeated line. A line in any other card section is ignored, as the hash
  lines are today.
- The request body must produce the same `after_sha256` (step 5), so the body,
  the card text and the hashes all describe one change.
- `--print-hash` prints the ready-to-post `decision` text, so the agent never
  hand-types the lines.

Reviewer procedure, before the owner is asked: run the same `--print-hash`
command against the live catalog and compare the output byte for byte with the
`decision` text of the posted request; read the `change` line as the change.
Remaining trust: the other card sections (context, "Agent did") are free text
and the owner should read the `change` line, not those.

## CLI

```text
multica workspace status list [workspace] [--include-archived]
multica workspace status create --key K --name N --category C [--color #rrggbb] [--icon I] [--description D]
multica workspace status update <key|id> [--name N] [--category C] [--color X] [--icon I] [--description D] [--confirm-category-change]
```

`create` and `update` share two flags:

- `--print-hash`: GET the live catalog, apply the change locally, print
  `before_sha256`, `after_sha256` and the ready-to-post `decision` text as
  JSON. Writes nothing.
- `--decision-id <uuid>`: same computation, then send the bound body. The CLI
  recomputes the hashes from the live catalog at that moment.

Without either flag the command makes the plain owner/admin request. A 403
without `--decision-id` explains that only an owner or admin can write statuses
and names the decision-bound path. Flags `--position` and archive/reorder do
not exist on the CLI.

## Tests (each child writes the red test first)

Server, `server/internal/handler/issue_status_bound_write_test.go`, modelled on
`issue_workflow_bound_write_test.go`:

| Test | Asserts |
| --- | --- |
| accepts approved create | 201, row stored, `decision_bound_write` row |
| accepts approved update | 200, fields changed |
| refuses without `decision_id` (task token) | 403, nothing written |
| refuses cloud PAT | 403 |
| refuses stale `before_sha256` | 409, decision unused |
| refuses wrong `after_sha256` | 403, decision unused |
| refuses reuse of a decision | 409 `decision already used` |
| approval does not carry to another workspace or field | 403 |
| refuses agent not on the allow list, or field not allowed | 403 |
| refuses non-owner answer, expired, non-approve answer | 403 |
| refuses `position`, missing `key` on create | 400 |
| card `change` line differs from the body's change (hashes match the body) | 403, decision unused |
| card with no, or a repeated, `change` or `recategorized_issues` line | 403 |
| `change` line only in another card section | 403 |
| issues assigned to the status after the ask (category change) | 403, body carries live `n`, decision unused |
| approved category change of 0 issues cannot apply once issues exist | 403 |
| refuses built-in status, key rename, archived status | 403 / 400 / 409 |
| category change needs `confirm_category_change` | 409 `category_change_requires_confirmation` |
| refuses reorder and delete for a task token | 403 |
| human owner path ignores decision fields | unchanged behaviour |

Pure, `server/internal/issuestatus/catalog_hash_test.go`: golden hash vector with and without `n`;
order independence; `n` changes the hash; position, id and timestamps do not change the hash; archived
rows do; HTML characters stay literal; `ApplyChange` matches what the handler
stores (colour lowercased, name trimmed).

CLI, `cmd/multica/cmd_workspace_status_test.go`: `--print-hash` writes nothing
and matches the server vector; `--decision-id` sends the bound body; a flag
combination the server refuses fails before any HTTP call; the plain path is
unchanged.

## Implementation split

Stacked, each at most 4 hours, test-first, project Cheese Work Setup:

| # | Issue | Depends on | Estimate |
| --- | --- | --- | --- |
| 1 (CHE-3097) | `issuestatus.CatalogHash` and `ApplyChange` (pure, golden vector) | none | 2 h |
| 2 (CHE-3100) | Bound mode on create/update, router middleware, `validateStatusFields`, new delegated field, agent actor on the event | 1 | 4 h |
| 3 (CHE-3103) | `multica workspace status` commands, `--print-hash`, `--decision-id`, skill reference (`decisions.md`, `workspace.md`) | 2 | 4 h |

## Relationship to CHE-3070

CHE-3097, CHE-3100 and CHE-3103 are follow-up issues, not sub-issues of
CHE-3070. CHE-3070 is design only. A sub-issue would hold it open, and block a
PASS, until the implementation ships. The acceptance "child implementation
issues filed" is met by filing them with project, estimate and test-first
criteria; each names CHE-3070 and the design PR as origin. They stay in backlog
until the design review passes.

## Risks and open questions

- A new status does nothing until a workflow lists it, and a machine caller
  cannot add it to a workflow without its own decision. Two approvals for one
  rollout is intended.
- A category change rewrites the machine behaviour of every issue on the
  status. The confirmation flag and the issue count in the ask are the guard.
  Whether to refuse category changes in bound mode altogether is a product
  choice; the default here is to allow them, with the count.
- Whether the web settings page for delegated writes lists fields from a
  constant or from the server is not checked. Child 2 greps for it.
