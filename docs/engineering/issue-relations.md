# Issue relations graph

The web and desktop issue sidebar loads the graph only after **Relations graph** is clicked. Closing it unmounts the renderer. Changing issues resets the open state. The API validator, query and rendering modules are lazy-loaded; the closed sidebar does not subscribe to a relations query.

`GET /api/issues/{id}/relations?depth=1` returns `nodes`, typed `edges`, `depth` and `omitted_count`. The default depth is 1. Only depths 1 and 2 are accepted. The server returns at most 40 nodes, choosing the current issue first and then nearest neighbors. The omitted count covers distinct reachable issues within the requested depth. Every edge endpoint must be in the returned node set. Traversal is bidirectional; arrows retain the source relation's direction.

| Type | Direction | Existing data |
| --- | --- | --- |
| `parent` | Parent → child | `issue.parent_issue_id` |
| `duplicate` | Duplicate → original | `issue.duplicate_of_issue_id`, while cancelled |
| `wakeup` | Watched → waiting | Enabled `issue_wakeup.condition`, `other_issue` |
| `children_done` | Child → parent to wake | Enabled `issue_wakeup.condition`, `children_done`; honors stage |
| `mention` | Mentioning → mentioned | `mention://issue/<uuid>` in descriptions and comments |
| `shared_pr` | Bidirectional | Shared `issue_pull_request.pull_request_id` |

The executable `other_issue` and `children_done` conditions are stored on `issue_wakeup`, not `issue_wakeup_definition`. Definitions configure the built-in presets and their inheritance; instances hold the conditions used here. Disabled wakeups do not indicate that an issue will wake another issue and are excluded. No rule is created, modified or executed by opening this graph. `issue_dependency` is not read.

The root requires workspace membership. Both endpoints of every edge are restricted to that workspace before traversal and counting. Wakeup edges use the same agent visibility filter as the wakeup listing endpoint. Cross-workspace targets and private wakeups do not contribute to the omission count. The database read has a 10-second request deadline; errors are reported rather than presented as an empty graph.

The node cap bounds the response, not total SQL work. The query materializes the workspace's existing edges before bounded-depth traversal. Very large workspaces can reach the request deadline; an indexed relation projection is a possible follow-up if query timings justify it.

Node labels encode Mermaid grammar delimiters as entities. The rendered SVG text nodes decode those numeric entities using textContent, never HTML. Only generated node names enter the diagram grammar. Mermaid retains `securityLevel: "strict"` and SVG text labels; no Mermaid click directives or issue-supplied markup is evaluated. The graph reuses the Mermaid block renderer's loader, theme and size helpers, plus the existing zoom canvas. React click and keyboard handlers resolve only nodes in the response. The existing generic Markdown Mermaid renderer keeps its sandboxed iframe.

Agents use `multica issue relations <id> [--depth 1|2] --output json` to read the same response, including reverse wakeup and mention edges. The command accepts a UUID or issue identifier and defaults to depth 1. It does not change issue state.

Run the focused checks from the repository root:

```sh
pnpm --filter @multica/core exec vitest run api/issue-relations.test.ts
pnpm --filter @multica/views exec vitest run issues/components/issue-relations-button.test.tsx issues/components/issue-relations-graph.test.tsx
pnpm --filter @multica/views exec vitest run issues/components/issue-detail.test.tsx -t 'offers a relations graph'
```

With a task-isolated database and the local backend/frontend configured through the checkout's environment:

```sh
(cd server && go test ./internal/handler -run '^TestIssueRelations' -count=1 -v)
pnpm exec playwright test e2e/issue-relations.spec.ts --project=chromium
```

The browser tests capture desktop and phone before/after states, verify click-only loading, depth changes, title escaping, node navigation, error recovery, empty state and focus restoration.

### Pre-click memory observation

The X99 check compared this candidate with main at `a62f684cc16ceee8290af2ba71dfc4b0f7b6a992`, using the same issue, backend, viewport and Chromium version. Each Next.js development preview used six fresh browser contexts. The first was discarded as warmup; retained JavaScript heap was measured after CDP garbage collection in the other five.

| Preview | Median retained heap |
| --- | ---: |
| Main | 117,267,068 bytes |
| Candidate | 117,443,608 bytes |
| Difference | +176,540 bytes (+0.15%) |

Neither preview requested relations data or the lazy relations-dialog chunk before the click. The small measured difference is not a claim of byte-identical RAM. Production-mode memory and whole-process RSS were not measured. The run's evidence bundle contains the samples, probe source and before/after PNGs.
