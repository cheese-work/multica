# Issue relations

The web and desktop issue sidebar shows a Hierarchy minimap (parent, siblings, children) built from the issue and child-issue lists, not from this API: the API caps at 40 nodes. The minimap reads relations at depth 1 only for the wake count. Its canvas dialog adds wake neighbours on the Wakes chip and mention neighbours on the Mentions chip; mentions are fetched on first use because they are expensive server-side.

`GET /api/issues/{id}/relations?depth=1` returns `nodes`, typed `edges`, `depth` and `omitted_count`. The default depth is 1. Only depths 1 and 2 are accepted. The server returns at most 40 nodes, choosing the current issue first and then nearest neighbors. The omitted count covers distinct reachable issues within the requested depth. Every edge endpoint must be in the returned node set. Traversal is bidirectional; arrows retain the source relation's direction.

| Type | Direction | Existing data |
| --- | --- | --- |
| `parent` | Parent → child | `issue.parent_issue_id` |
| `duplicate` | Duplicate → original | `issue.duplicate_of_issue_id`, while cancelled |
| `wakeup` | Watched → waiting | Enabled `issue_wakeup.condition`, `other_issue` |
| `children_done` | Child → parent to wake | Enabled `issue_wakeup.condition`, `children_done`; honors stage |
| `mention` | Mentioning → mentioned | `mention://issue/<uuid>` in descriptions and comments |
| `dependency` | Dependent → depends-on | `issue_dependency` (`issue_id` → `depends_on_issue_id`), any dependency type |
| `shared_pr` | Bidirectional | Shared `issue_pull_request.pull_request_id` |

The executable `other_issue` and `children_done` conditions are stored on `issue_wakeup`, not `issue_wakeup_definition`. Definitions configure the built-in presets and their inheritance; instances hold the conditions used here. Disabled wakeups do not indicate that an issue will wake another issue and are excluded. No rule is created, modified or executed by opening this graph.

The root requires workspace membership. Both endpoints of every edge are restricted to that workspace before traversal and counting. Wakeup edges use the same agent visibility filter as the wakeup listing endpoint. Cross-workspace targets and private wakeups do not contribute to the omission count. The database read has a 10-second request deadline; errors are reported rather than presented as an empty graph.

The node cap bounds the response, not total SQL work. The query materializes the workspace's existing edges before bounded-depth traversal. Very large workspaces can reach the request deadline; an indexed relation projection is a possible follow-up if query timings justify it.

Agents use `multica issue relations <id> [--depth 1|2] --output json` to read the same response, including reverse wakeup and mention edges. The command accepts a UUID or issue identifier and defaults to depth 1. It does not change issue state.

Run the focused checks from the repository root:

```sh
pnpm --filter @multica/core exec vitest run api/issue-relations.test.ts
pnpm --filter @multica/views exec vitest run issues/components/hierarchy
```

With a task-isolated database:

```sh
(cd server && go test ./internal/handler -run '^TestIssueRelations' -count=1 -v)
```
