# Fixes and improvements for upstream

Companion to [FORK-FEATURES.md](FORK-FEATURES.md) and [UPSTREAM-READINESS.md](UPSTREAM-READINESS.md) (CHE-2792).
Covers every merged fork PR whose title type is `fix` or `perf` (287 PRs, grouped by bug into 251 fixes). It does not authorize a PR, merge or deployment.

## Method

- A fix is one bug, which can span several fork PRs (follow-up fixes to the same bug are grouped).
- **Code area**: `upstream-shared` if the changed non-test files exist in upstream `main`; `fork-only` if the code is fork-only. Fork-only fixes get one line each in [Stays in fork](#stays-in-fork).
- **Upstream state** for upstream-shared fixes, checked against upstream `main` `73975653323750d04c27047ea9c986ef06bdf6a1` (2026-11-08 13:35 ICT):
  `present` = the bug is still in upstream (evidence cites the upstream file and line); `fixed upstream` = upstream fixed it (evidence cites the commit); `unknown` = could not be decided (the reason says why).
- **Size** = changed lines outside test files. **Regression test** = `yes` only when a test the fix PRs added or changed asserts the fixed behavior (test names and assertions read in the PR diff; not run here); the cell names it. `partial` = a test covers part of the fix. `unverified` = test files were touched but their coverage of the bug was not read. `no` = no test file touched. A touched test file alone is never `yes`.
- **Depends on** = ledger rows the fix needs, hard or soft, with one pointer; `none` = independent.
- Evidence was read at the upstream pin `73975653323750d04c27047ea9c986ef06bdf6a1` (2026-11-08 13:35 ICT; tag v0.8.1 is `b1ea927def6490c43bedff537b6f4ed9a47fa828`, 2026-11-06 16:51 ICT, and the pin is 15 commits ahead of it). Since the previous pin `3794359792e1e310d89d0488b46efff66e9d2369` (2026-11-02 19:02 ICT), 134 upstream commits changed 1053 files. All 37 earlier non-fork-only rows were re-read at the new pin: FX083 moved from `present` to `fixed upstream` (conditional, see its row), 11 evidence lines moved or were reworded (FX036, FX070, FX058, FX074, FX077, FX080, FX082, FX090, FX106, FX137, FX148) and no other status changed. The 92 fix and perf fork PRs of this refresh (CHE-4789) were read at the same pin. Fork `main` for this refresh is `e8f066f3eaba3a2f3ea2f832b2a962e4356e310a` (2026-11-09 08:44 ICT).
- **Security-class rows** (a defect upstream has not patched that is an authorization, credential or data-exposure gap) carry the id and a one-line effect only. They never carry upstream file or line, route names or exploit detail; the evidence cell reads `security-class; details withheld until upstream fixes it (private report pending)`. Report such a defect to upstream privately first (CHE-3676), then fill in the evidence after upstream fixes it. Git history keeps older text; it is not rewritten.
- Order of the `present` list: independent and test-backed first, then smaller first. Static read; no fix was re-run against upstream.

## Totals

| Class | Fixes | PRs |
| --- | --- | --- |
| Upstream-shared, `present` | 57 | 60 |
| Upstream-shared, `fixed upstream` | 5 | 5 |
| Upstream-shared, `unknown` | 10 | 12 |
| Fork-only (stays in fork) | 179 | 210 |
| Total | 251 | 287 |

## Present in upstream, ordered to pitch

| # | ID | Fix | Fork PRs (CHE) | Size | Regression test | Depends on | Upstream evidence | Ledger row |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | FX045 | Failed issue-mention hydration never recovers on focus | #199 (CHE-1569) | 1 | yes: `issue-chip-resolution.test.tsx` (refetch on focus) | none | upstream/main packages/views/issues/components/issue-chip.tsx:67-70 reads the query result without refetch-on-focus; packages/core/query-client.ts:10-17 sets refetchOnWindowFocus false and retry limits, so a failed mention fetch stays failed (same pattern the fork 1-line fix removed) | C21 |
| 2 | FX217 | Mermaid viewer swallows Ctrl/Cmd +/-/0 browser-zoom shortcuts | #1027 (CHE-3966) | 3 | yes: `leaves browser-zoom shortcuts (Ctrl/Cmd + - = 0) to the browser` | none | upstream/main packages/views/editor/hooks/use-zoom-canvas.ts:356 handleKeyDown matches "+"/"="/"-"/"_"/"0" and calls preventDefault with no ctrlKey/metaKey check | C46 |
| 3 | FX239 | iOS Safari copy fallback selects no text (hardening; reported %20 bug did not reproduce) | #1166 (CHE-4386) | 3 | yes: `fallback sets an explicit selection range, which iOS Safari needs` | none | upstream/main packages/ui/lib/clipboard.ts:67 textarea.select() with no setSelectionRange | - |
| 4 | FX081 | PR merge skips issues in a custom done-category status | #371 (CHE-1990) | 8 | yes: `handler/github_test.go` TestWebhook_MergedPR_MovesAgentAccepted | none | upstream/main server/internal/handler/pr_auto_complete.go:191-196 `effective := resolver.Effective(...); case effective == issuestatus.Done // effective == issuestatus.Cancelled: terminal` - Effective folds any custom done/cancelled-category status into done/cancelled (issuestatus.go:639-648 customBehavior), so merge never moves such an issue to the configured merge target. Fork fix compares the literal built-in key. Policy-ish: upstream comment shows the folding is deliberate. | - |
| 5 | FX131 | Task tokens can edit/delete issue workflows | #573 (CHE-2449) | 10 | yes: `cmd/server/issue_workflow_task_token_test.go` | none | security-class; details withheld until upstream fixes it (private report pending) | - |
| 6 | FX219 | In-app issue link labelled with its own identifier renders as a plain link, not the issue chip | #1042 (CHE-3995) | 11 | yes: `renders a link labelled with its own issue identifier as an issue chip` (`entity-link-unfurl.test.tsx`) | none | upstream/main packages/views/rich-content/rich-content.tsx:203 returns null unless childrenToLabel(children) equals href, so an identifier label never unfurls | - |
| 7 | FX166 | Code-block copy button showed only on hover, so touch screens never saw it, and it covered code; it now shows on touch and no longer covers code | #769 (CHE-2939) | 12 | yes: `is revealed on touch screens, which have no hover (CHE-2939)` | none | `packages/ui/markdown/CodeBlock.tsx:196` `opacity-0 group-hover:opacity-100` (same on `packages/views/editor/extensions/code-block-view.tsx:104`, `packages/views/rich-content/rich-code-block.tsx:142`) | none |
| 8 | FX120 | Codex effort dropped for explicit model absent from the runtime catalog | #538 (CHE-2373) | 17 | yes: `daemon/thinking_level_warning_test.go` TestResolveTaskModelSelectionCodexUnlistedEffort | none | upstream/main server/pkg/agent/thinking.go:798 `if providerType == "claude" && model != "" && ...` returns errUnverifiedCatalog (pass-through) only for Claude; a Codex model omitted from discovery falls to :801 `model %q was not found in the runtime catalog`, rejecting the saved effort. | - |
| 9 | FX061 | WeCom inbox acceptance wait overruns its deadline | #298 (CHE-1841, CHE-1842) | 18 | yes: `wecom/relay_confirm_overshoot_test.go` (last wait stops at the deadline) | none | upstream/main server/internal/integrations/wecom/relay_outbound.go:1030 and :1033 poll loop does not end at its deadline (same pattern the fork fix removed); patch hunk 2 applies at offset +46, hunk 1 needs context rebase | D12 |
| 10 | FX105 | Add comment popup does not open when mouse-up lands outside the message text | #458 (CHE-2206) | 19 | yes: `use-comment-annotations.chat.test.tsx` "selection released outside the message text" | none | upstream/main use-comment-annotations.tsx:239-242 captureProps.onPointerUp captures only if event.target.closest('[data-comment-content]'); a drag released in padding/gutter never calls capture(). Fork fix arms on pointerdown and captures on document pointerup. | - |
| 11 | FX204 | The iOS keyboard left the DM composer capped at 50% of the layout viewport; it is now capped at 40% of the visible viewport while the keyboard is open | #968 (CHE-3534) | 19 | yes: `caps the composer at 40% of the visible viewport while the keyboard is open` | none | `packages/views/chat/components/chat-input.tsx:639-645` `max-h-[50%]`, no visualViewport cap (`use-visual-viewport-keyboard.ts` exists upstream but is not applied here) | none |
| 12 | FX133 | Claude-injected turn result closes stdin early | #580 (CHE-2462) | 25 | yes: `agent/claude_result_test.go` TestClaudeExecuteTurnResult | none | upstream/main server/pkg/agent/claude.go:327-358 case "result" accepts any result: sets sawResult/finalResultText and calls closeAfterControls() with no check that the result belongs to the daemon prompt; no user_message_uuid/promptUUID in file (grep empty). A resumed CLI result from an injected task-notification turn closes stdin on the pending prompt. | - |
| 13 | FX187 | A transient issue-fetch failure ended in a load error; fetches now retry 3 times with 1s/2s/4s backoff and never retry a 404 | #930 (CHE-3535) | 33 | yes: `renders the issue when network failures are followed by success`, `does not retry a 404` | none | `packages/core/issues/queries.ts:516-521` `issueDetailOptions` has no retry policy; app default is `retry: 1` (`packages/core/query-client.ts:14`) | none |
| 14 | FX060 | Dev env startup GC destroys containers without opt-in | #280 (CHE-1614, CHE-1813) | 35 | yes: `scripts/dev-env.test.sh` (startup runs no GC without opt-in; stub repo) | none | upstream/main scripts/dev-env.sh:1130 and :1438 still run the destructive startup GC unconditionally; dry-run patch applies cleanly to dev-env.sh and Makefile | O19 |
| 15 | FX230 | Add-comment popup misses triple-click on last block and gutter-start drags | #1124 (CHE-4287) | 36 | yes: `captures a triple-click on the last block, whose range ends after the content root`; `opens the action when a drag starts in the gutter and ends in the text` | none | upstream/main packages/views/issues/components/use-comment-annotations.tsx:240-244 onPointerUp needs the target inside [data-comment-content]; comment-annotation-selection.ts:38-39 requires equal start and end roots; extends FX105/FX088/FX129 | C20 |
| 16 | FX080 | --no-start (suppress_run) drops the workflow step handoff run for a different handler | #369 (CHE-1948, CHE-1966, CHE-1984, CHE-1988) | 40 | yes: `handler/issue_workflow_no_start_test.go` TestNoStartSuppressesHandoffRunForEveryHandler (CHE-4964: #369 exception reversed, --no-start now suppresses the handoff run too) | none | upstream/main:server/internal/handler/issue.go:4171 `willRun = willRun && !req.SuppressRun` (single) and :4969 `willRun = willRun && !req.Updates.SuppressRun` (batch) while a workflow handoff (workflowHandoff, issue_workflow_entry.go:35) reassigns to another agent; so the flag silences the new handler's run, same pattern the fork replaced with suppressRunForWrite. Fork code used (handedToAgent, issue_workflow_entry.go:189) also exists upstream. | - |
| 17 | FX106 | Mobile share/copy links use web-404 path /{ws}/issue/<id>; web has no /issue/ redirect | #460 (CHE-1798, CHE-2202) | 41 | partial: `apps/web/proxy.test.ts` covers the web redirect; the mobile link builders are untested | none | upstream/main:apps/mobile/app/(app)/[workspace]/issue/[id].tsx:120 and apps/mobile/components/issue/comment-context-menu.tsx:163 still build `${webUrl}/${wsSlug}/issue/${identifier}`; web only has apps/web/app/[workspaceSlug]/(dashboard)/issues/[id] and apps/web/proxy.ts LEGACY_ROUTE_SEGMENTS has no 'issue', so the shared link 404s (same pattern fork changed to /issues/). PR also mixes fork-only decision-link builders (decisiontx/ask.go, decision_record.go, decision_request_api.go, decision_veto_notice.go, util/issue_link.go) that belong in the fork lane. | - |
| 18 | FX172 | Realtime invalidation refetched queries in hidden tabs on every event; invalidation now defers until the tab is visible and then fetches once | #820 (CHE-3117) | 46 | yes: `hidden tab defers refetch until visible, then fetches once` | none | `packages/core/query-client.ts:6-19` no visibility handling; `packages/core/realtime/use-realtime-sync.ts:152-153,174` invalidate unconditionally | none |
| 19 | FX238 | Copy-feedback timers fire after unmount in code block, table and table dialog | #1164 (CHE-4405) | 47 | yes: `clears the copy-feedback timer on unmount`; `starts no feedback timer when unmounted before the copy resolves` | none | upstream/main packages/views/rich-content/rich-code-block.tsx:136, rich-table.tsx:387, rich-table-dialog.tsx:72 call setTimeout without clear or unmount guard | - |
| 20 | FX191 | In-app links, docs and OpenSearch URLs used the configured app URL, wrong for visitors on another origin; links now use the visitor's origin | #929 (CHE-3536) | 50 | yes: `uses the visitor's own origin when it differs from the configured public origin` | none | `packages/core/workspace/workspace-url.ts:19-34` `workspaceUrlHost` uses only `daemon_app_url`, no `location.origin` | none |
| 21 | FX251 | Autolinked URL keeps trailing GFM punctuation (colon, period, comma, unbalanced paren) | #1264 (CHE-4660) | 52 | yes: `excludes the ':' in the original bug shape` (`preprocess-links.test.ts`) | none | upstream/main packages/ui/markdown/linkify.ts:43 TRAILING_MD_DELIMITER = /[*~]+$/ trims only * and ~; other trailing punctuation stays in the URL (applied ~315-319) | - |
| 22 | FX082 | Agent PRs not linked to their issue (branch name and Refs/Part of keywords) | #373 (CHE-7, CHE-1988, CHE-1991) | 55 | yes: `repocache/cache_test.go` TestCreateWorktreeBranchCarriesIssueIdentifier | none | upstream/main server/internal/daemon/repocache/cache.go:894 `branchName := fmt.Sprintf("agent/%s/%s", sanitizeName(params.AgentName), taskKey(params.TaskID))` (no issue key in the branch), and server/internal/handler/github.go:1069 closingIdentifierRe only accepts close/fix/resolve keywords; a branch/body without those links nothing. Task.IssueIdentifier exists upstream (daemon/types.go:88). Enhancement more than defect; mixes in one fork-only line (issue_workflow_ci_advance.go workflowHandoffPayload arg, W04) that must be dropped. | - |
| 23 | FX027 | Codex SQLite state-runtime init exit not retried | #101 (CHE-737) | 56 | yes: `agent/codex_test.go` TestCodexExecuteRetriesOnceAfterSQLiteStateRuntimeInitExit | none | upstream/main server/pkg/agent/codex.go:~1535 retrySafe := timedOut && !semanticObserved.Load() && cleanupConfirmed && codexInitializeRetrySupported(): retry only on timeout, so an initialize exit from SQLite state-runtime init is not retried | - |
| 24 | FX231 | Comment and reply composers keep their text after a send that remounts the page; trailing blank lines block clear | #1126 (CHE-4288) | 59 | yes: `a %s composer remounted mid-send clears when the send succeeds`; `a tab switch during the %s send clears a body with trailing blank lines` (`comment-composers.test.tsx`) | none | upstream/main packages/views/issues/components/comment-input.tsx:126 flushes getMarkdown() untrimmed into the draft; reply-input.tsx same; no clear-on-draft-consumed hook exists | - |
| 25 | FX244 | checks_finished wakeup never fires when checks already finished on the current head | #1187 (CHE-4444) | 59 | yes: `TestBaselineChecksFinishedOnCurrentHead`; `TestWakeupConditionPullRequestChecksFinishedOnCurrentHead` | none | upstream/main server/internal/service/issue_wakeup_condition.go:537-548 conditionFiresOnChange and baselineCondition ignore already-finished checks; --until-pr checks exists upstream (cmd_issue_wakeup.go:294) | W04 |
| 26 | FX193 | Chat list start index came from page counts, so a refetch that only re-paged the same messages shifted the list; the index now derives from prepended messages | #939 (CHE-3232) | 69 | yes: `keeps the index when a refetch only re-pages the same messages` | none | `packages/views/chat/components/use-chat-controller.ts:249-252` `firstItemIndex = CHAT_VIRTUOSO_INITIAL_FIRST_ITEM_INDEX - olderMessageCount` (page-count formula) | none |
| 27 | FX196 | After a reconnect, event-fed caches (pins, invitations, notification preferences, integration inventories, VCS connections, PR lists, comment-trigger previews) stayed stale; reconnect now invalidates them | #944 (CHE-3574) | 82 | yes: `invalidates event-fed account and integration caches on reconnect` | none | `packages/core/realtime/use-realtime-sync.ts:651-700` `invalidateWorkspaceScopedQueries` omits those caches; reconnect handler at 1942-1952 | none |
| 28 | FX195 | A half-open WebSocket stayed dead after a tab returned; the client now probes on visibility, online and pageshow and reconnects when no pong arrives | #943 (CHE-3573) | 102 | yes: `reconnects a half-open socket when a probe gets no pong` | none | `packages/core/api/ws-client.ts` has no visibilitychange, online, pageshow, probe or pong handling; reconnect state at lines 43-50 | none |
| 29 | FX137 | Wakeup run inherits a stale workflow step from its registering run | #599 (CHE-2553) | 112 | yes: `service/issue_wakeup_workflow_test.go` TestWakeupFromStaleRunWorksOnTheIssuesStatus | none | upstream/main server/pkg/db/queries/wakeup.sql:206-210 CreateWakeupTask sets workflow_step = COALESCE((SELECT s.workflow_step FROM agent_task_queue s WHERE s.id = delegated_from_task_id ...), issue status): a wakeup copies the registering run step at fire time, so a run already stale when it registers hands that step down every wakeup chain and the guard (issue_workflow_entry.go:158 workflow_step_moved) refuses every status change. | - |
| 30 | FX074 | Repeat delivery of an already-executing task is re-run | #358 (CHE-1972) | 118 | yes: `daemon/duplicate_delivery_test.go` TestRunBatchPollerIgnoresDuplicateDeliveryOfInflightTask | none | upstream/main server/internal/daemon/daemon.go:5470 calls d.handleTask(parentCtx, t, slot) with no running-task guard; server/pkg/db/queries/agent.sql:882-896 claim can redeliver. Patch applies cleanly to daemon.go | D08 |
| 31 | FX058 | DM back stack lost on web and desktop | #267 (CHE-1755, CHE-1758) | 120 | yes: `core/chat/store.test.ts` navIntent; `views/chat/chat-page.test.tsx` | none | upstream/main packages/views/chat/chat-page.tsx:126 still has single-state chat navigation without a back stack (store/index.ts shape matches the fork pre-fix); replace() also at :232, :238. Patch applies with drift: store.ts and index.ts clean, chat-page.tsx hunks 1,2,5,11 need manual rebase | C17 |
| 32 | FX209 | OAuth and Composio callbacks redirect to FRONTEND_ORIGIN, not the origin the user started on | #987 (CHE-3622) | 126 | yes: `TestGitHubSetupCallbackReturnsToStartingOrigin`; `TestComposio_CallbackReturnsToStartingOrigin` | none | upstream/main server/internal/handler/github.go:508 uses only strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN")) as the redirect origin (UX bug, not security-class) | - |
| 33 | FX118 | Redactor masks shell references and placeholders as credentials | #531 (CHE-2338, CHE-2340) | 155 | yes: `redact/redact_test.go` TestGenericCredentialValues; `task-transcript/redact.test.ts` | none | upstream/main server/pkg/redact/redact.go:71 `(?i)(?:API_KEY/.../TOKEN)\s*[=:]\s*\S+` -> [REDACTED CREDENTIAL] and packages/core/security/redact-secrets.ts:32 same regex; `TOKEN=$TOKEN`, `TOKEN=$(jq ...)`, `TOKEN=<token>`, `password: ***` are all masked, losing readable commands in transcripts. Fork TS change targets packages/views/.../redact.ts which upstream turned into a re-export, so the TS half must be ported into core/security/redact-secrets.ts. | - |
| 34 | FX174 | Frontend lint warnings (exhaustive-deps, unstable hook deps, stepper focus); cleared across the web packages | #825 (CHE-3132) | 186 | partial: `focuses the replacement node after render swaps the trigger's button` (lint warnings themselves have no test) | none | `packages/core/platform/core-provider.tsx:157-158` `useMemo(() => initCore(...))` under `eslint-disable-next-line react-hooks/exhaustive-deps`; `packages/views/agents/components/tabs/mcp-config-tab.tsx:113-116` un-memoized `assignedServers` as a memo dep. Stepper part is not applicable upstream (`stepper.tsx:90` already uses `useCallback`) | none |
| 35 | FX215 | Mobile issue screen shows a connection error when a 200 response fails schema validation (medium confidence) | #1024 (CHE-3949) | 219 | yes: `says the data is unreadable, with the issue key and Retry, when a 200 fails schema validation`; `throws a typed error naming the failing path instead of returning an empty issue` | none | upstream/main apps/mobile/app/(app)/[workspace]/issue/[id].tsx:210-214 shows errors.load_failed (message or not-found) for any error; no SchemaValidationError at the pin; packages/core/api/client.ts uses parseWithFallback | - |
| 36 | FX050 | Decided open/draft PR snapshots that no conflict rule watches are never re-fetched | #230 (CHE-1395, CHE-1417, CHE-1665) | 280 | yes: `ghsnapshot/recheck_che1665_test.go` (bounded drain through sweepOnce); the catch-up after start has no test | none for the re-check half (query ClaimGitHubPRsDueForRecheck, migration 623, refresh.go sweepRecheck). The wake-rule half (BEHIND/draft/dedupe keys in handler/github.go and service/issue_wakeup_system.go) is fork-only PR-attention code and needs hard: W05; leave it out of the pitch | upstream/main (7397565332) partly fixed it, not fully. server/pkg/db/queries/github_snapshot.sql:46-70 ListStaleUndecidedGitHubPRs still drops a decided snapshot ("A decided snapshot leaves the undecided sweep"); :85-106 ListStaleConflictWatchedGitHubPRs re-checks a decided snapshot only when an enabled `issue_wakeup` rule with condition event `conflicts` links to it (`w.enabled`, :100-105); ghsnapshot/refresh.go:557 is its only caller. A decided open/draft PR with no such rule keeps a stale snapshot indefinitely, so the owner of a PR whose base moved by direct push is not woken. Upstream commit e720fe2219 covers the watched subset only | H05 |
| 37 | FX103 | Table auto-fit remeasured on every remount; thread rail looks up all rows each frame | #455 (CHE-2082, CHE-2174, CHE-2192) | 292 | yes: `rich-table.test.tsx` "auto-fit cost"; `thread-minimap.test.tsx` | none | upstream/main rich-content/rich-table.tsx:129,224-240 measureColumns() runs on every mount, no cache or row-count cap (fork fix caches per content+width, skips >50 rows); thread-minimap.tsx:230-234 loops document.getElementById over all anchorIds each frame (rail was refactored upstream, so that half needs re-cut). | - |
| 38 | FX170 | Issues list and issue detail fired duplicate and early background requests (detail re-read after lookup, property list keyed by `includeArchived`, eager modals and chat window); one request now serves the lookup and detail, the property list shares one request, windows load on demand | #802 (CHE-3020) | 393 | partial: `does not re-read a resolved issue when the detail view mounts on it`, `serves live-only and archived-inclusive readers from one request` (case-level asserts of the parts that remain) | none | Partly fixed upstream (`packages/core/issues/canonical-id.ts:52-58` single-request `initialData`). Still present: `packages/core/properties/queries.ts:6-13` list keyed by `includeArchived` (two requests); `packages/views/issues/components/issue-detail.tsx:1406` `useQuery(issueListOptions(wsId))`; `packages/views/modals/registry.tsx:5-17` static modal imports. Chat window, autopilot dialog and assignee picker parts not individually checked | none |
| 39 | FX169 | Web startup loaded the full zod schema graph, all modals, the chat window, search, issue detail and shiki eagerly; each now loads on first use or first intent, so route JS and pre-LCP bytes drop | #798 (CHE-3019), #803 (CHE-3018), #914 (CHE-3021) | 1886 | partial: `construct nothing at module load`, `loads each modal on first open, not at startup`, `loads shiki when a code block renders, not when the module is imported` (import-graph asserts; no bundle-size assert) | none | `packages/core/api/schemas.ts:1465` eager `IssueSchema` (1671 `z.` uses); `packages/core/api/client.ts:5,521` eager schema import; `packages/views/modals/registry.tsx:5-17` static modal imports; `packages/views/layout/dashboard-layout.tsx:5-6` static `ModalRegistry` and `SourceBackfillModal`; `packages/ui/markdown/CodeBlock.tsx:2` static `shiki` import | none |
| 40 | FX134 | Mermaid xychart/timeline/pie/radar unreadable in dark mode | #590 (CHE-2294, CHE-2525) | 41 | yes: `editor/utils/mermaid-theme.test.ts` | hard: C46 `packages/views/editor/utils/mermaid-theme.ts` | upstream/main packages/views/editor/mermaid-diagram.tsx:99-118 getMermaidThemeVariables sets only primaryColor/primaryBorderColor/primaryTextColor/lineColor; no darkMode, xyChart, cScale/pie or radar variables (git grep cScale/xyChart upstream/main -- packages empty), so those diagram types keep Mermaid light defaults in dark mode. | - |
| 41 | FX076 | Auto-update never installs while the daemon has a running task | #361 (CHE-1978, CHE-1979) | 57 | yes: `daemon/auto_update_fork_test.go` (installs while busy) | soft: D02 `fork diff calls d.autoUpdateTarget(ctx) (fork-only, introduc` | upstream/main server/internal/daemon/auto_update.go:214-217 `if running := d.activeTasks.Load(); running > 0 { ... return }` before fetchLatestRelease, then trySetClaimBarrier at :253 defers whole upgrade when busy; same pattern the fork removed (install now, only restart waits for idle barrier). A daemon that is always busy never updates. | - |
| 42 | FX129 | Annotation note popup loses draft on touch scroll and does not size on Safari | #571 (CHE-2428) | 58 | yes: `use-comment-annotations.chat.test.tsx` "annotation note drafts" | soft: C20 `(use-comment-annotations.tsx` | upstream/main packages/views/issues/components/use-comment-annotations.tsx:79-84 pointerdown dismisses (setSelection(null); setEditor(null)) for every pointer type incl. touch, and the unconfirmed note lives only in editor state that dismissal drops; :268-271 textarea relies solely on CSS field-sizing-content (no fallback). Same pattern the fork fix replaced. | - |
| 43 | FX148 | Issue detail back-navigation restores wrong comment (pixel offset on virtualized timeline) | #673 (CHE-1857, CHE-2668) | 128 | yes: `use-issue-detail-scroll-anchor.test.ts` (anchor read); `e2e/issue-detail-back-navigation.spec.ts` not run in default CI | none | upstream/main packages/views/issues/components/issue-detail.tsx:3996 Virtuoso initialScrollTop={restoredScrollTop} restores a stored pixel offset (useRestoredScrollOffset, :1490); row heights are estimated until measured, so remount lands on a different comment. Fork e2e: scrollTop 7680 before leaving, 13385 after Back. Not run against upstream. | - |
| 44 | FX088 | Annotation 'Add comment' popup: IME/keystroke lag, iOS menu overlap, hidden under keyboard | #393, #430 (CHE-2077, CHE-2162) | 136 | yes: `use-comment-annotations.ime.test.tsx`; `use-comment-annotations.chat.test.tsx` | soft: C20 `use-comment-annotations.tsx` | upstream/main packages/views/issues/components/use-comment-annotations.tsx:268,273 note textarea is controlled by hook state (value={editing.note}, onChange -> setEditor), so each keystroke/IME update re-renders the whole host page; comment-selection-bubble.tsx:35,51 fixed placement 'top' + offset(8), no pointer:coarse handling (iOS native menu sits above selection) and no visualViewport/keyboard handling. Same pattern the fork fix removed. | - |
| 45 | FX198 | Unsent annotation notes, wakeup forms and issue title edits were lost on unmount; they persist in the keyed draft store | #957 (CHE-3522) | 142 | yes: `restores an unsent title edit after the page unmounts and remounts` | hard: C63 `packages/core/drafts/create-keyed-draft-store.ts` | `packages/views/issues/components/wakeup-create.tsx:156` draft in local `useState`; `packages/views/issues/components/use-comment-annotations.tsx:27-28` refs | C63 |
| 46 | FX070 | PR close-link repair from CLI | #339 (CHE-1930, CHE-1932, CHE-1933) | 183 | partial: `cmd/multica/cmd_issue_test.go` covers CLI routing and unlink only | soft: H04 `relies on fork actor-aware PR linking and unlink repair` | upstream/main server/cmd/server/router.go:2166 and server/internal/handler/pr_auto_complete.go:536 still auto-complete from PR text with no CLI repair path for wrong close links (new e7592c57ed adds only a per-issue --pr-auto-complete on/off toggle, PUT /pr-auto-complete router.go:2167); patch applies cleanly to cmd_issue.go | H04 |
| 47 | FX100 | Oversized comment bodies and tables mount synchronously on minimap jump | #447 (CHE-2174, CHE-2191, CHE-2272) | 186 | yes: `comment-card-long-body.test.tsx` | hard: C01 `comment-card.tsx` | upstream/main comment-card.tsx:909,1459 `<ReadonlyContent content={entry.content ?? ""} .../>` mounts the full body with no size cap; rich-table.tsx has no near-viewport deferral. Perf plus a visible behavior change (prefix + 'Show full comment' over 16,000 chars; Cheese accepted for the fork). Mixes an unrelated use-in-page-find keepRangeInView settle loop. | - |
| 48 | FX094 | Offscreen Mermaid/HTML previews stay mounted forever | #432 (CHE-2082, CHE-2132, CHE-2164) | 218 | yes: `editor/mermaid-diagram.test.tsx` MermaidDiagram lifecycle | soft: C46 `mermaid-diagram.tsx` | upstream/main rich-content/lazy-rich-block.tsx:24-26,103 'MOUNT-ONCE LATCH ... never unmounted' plus mounted-block-registry.ts; blocks scrolled past keep their Mermaid SVG/iframe. Deliberate upstream trade-off, so upstream may reject; also upstream issue-detail does not provide RichContentScrollRootProvider (only chat-message-list.tsx:354 does). | - |
| 49 | FX201 | Unsent squad instructions and inline field edits were lost on unmount; they persist in the keyed draft store | #962 (CHE-3526) | 225 | yes: `instructions typed inside the debounce window survive an immediate unmount` | hard: C63 `packages/core/drafts/create-keyed-draft-store.ts` | `packages/views/squads/components/squad-detail-page.tsx:731-745` `SquadInstructionsSection` holds `editing` and `value` in local `useState` | C63 |
| 50 | FX203 | Unsent workspace, account, invite and wakeup-rule form input was lost on remount; it persists in the keyed draft store | #964 (CHE-3527) | 282 | yes: `restores the profile name and description after unmount` | hard: C63 `packages/core/drafts/create-keyed-draft-store.ts` | `packages/views/settings/components/workspace-tab.tsx:142-143` and `account-tab.tsx:40-41` local `useState` | C63 |
| 51 | FX199 | Unsent skill and workflow editor input was lost on dialog close or unmount; it persists in the keyed draft store | #958 (CHE-3524) | 333 | yes: `keeps the typed name and description across a dialog close and reopen` | hard: C63 `packages/core/drafts/create-keyed-draft-store.ts` | `packages/views/skills/components/create-skill-dialog.tsx:119-120` name and description in local `useState` | C63 |
| 52 | FX090 | Issue timeline: deep link and find drop virtualization and mount the full list | #409 (CHE-2082, CHE-2132, CHE-2135) | 348 | yes: `issue-detail.test.tsx` (virtualized deep link and find) | soft: C01 `issue-detail.tsx` | upstream/main issue-detail.tsx:1943 `const isFlatTimeline = !!highlightCommentId // find.open;` still switches the whole timeline to a flat full-list render for deep links and in-page find (extra flat scroll paths at :1986-2011); use-in-page-find.ts walks mounted DOM only. Perf cliff on very long threads, same as base of the fork fix. Perf/behavior change, not a crash bug. | - |
| 53 | FX092 | Long thread replies mount together inside one oversized root row | #416 (CHE-2082, CHE-2132, CHE-2149) | 377 | yes: `issue-detail.test.tsx` "virtualizes replies as individual rows" | hard: C01 `issue-detail.tsx`; soft: C20 `416 adds revealAnnotation / pendingAnnotationEdit in issue-d` | upstream/main comment-card.tsx:1503,1521 `{threadRows.map(renderThreadRow)}` renders every reply inside the root's single Virtuoso item, so an oversized thread mounts all replies. Same pattern the fork fix split into per-reply rows. Perf, mixed with annotation-reveal and find-effect changes. | - |
| 54 | FX202 | Unsent agent instructions and settings were lost on unmount, and MCP drafts could keep credentials; they persist and credential-named keys are stripped | #963 (CHE-3525) | 551 | yes: `drops credential-named keys at any depth and keeps the rest` | hard: C63 `packages/core/drafts/create-keyed-draft-store.ts` | `packages/views/agents/components/tabs/instructions-tab.tsx:24` and `custom-args-tab.tsx:61-62` local `useState` | C63 |
| 55 | FX205 | Mobile issue and project edit and create forms lost unsent input on kill or navigation; drafts persist and clear only on a successful save or explicit discard | #969 (CHE-3529) | 651 | yes: `keeps the draft after a failed save and drops it after a successful one` | hard: C63 `apps/mobile/data/draft-persistence.ts` | `apps/mobile/app/(app)/[workspace]/issue/[id]/edit.tsx:52` and `project/[id]/edit.tsx:39-40` local `useState` | C63 |
| 56 | FX206 | Mobile comment and chat composers lost unsent text, replies, mentions and attachments on cold start; they persist per issue or chat session | #970 (CHE-3528) | 714 | yes: `comment composer: typed text` | hard: C63 `apps/mobile/data/draft-persistence.ts` | `apps/mobile/data/stores/chat-drafts-store.ts:3-5` in-memory store, "lost on app cold start"; `apps/mobile/components/composer/message-composer.tsx:185` local `useState` | C63 |
| 57 | FX200 | Unsent squad, autopilot and quick-action form input was lost on close; it persists in the keyed draft store and a flush lands in the originating workspace | #960 (CHE-3523) | 871 | yes: `lands an editor flush in the originating workspace, never the current one` | hard: C63 `packages/core/drafts/create-keyed-draft-store.ts` | `packages/views/modals/create-squad.tsx:73-74` and `packages/views/autopilots/components/autopilot-dialog.tsx:145-146` local `useState` | C63 |

## Fixed upstream

| ID | Fix | Fork PRs (CHE) | Size | Upstream commit | Ledger row |
| --- | --- | --- | --- | --- | --- |
| FX036 | GPT-6 Sol/Luna usage pricing missing | #143 (CHE-881) | 14 | upstream ba30324e10 "fix(codex): add GPT-6.1 Sol fallback and current model pricing (MUL-7834) (#8996)"; upstream pricing.go:39-40 and packages/views/runtimes/utils.ts:290-291 carry gpt-6-sol/gpt-6-luna rows | - |
| FX043 | govulncheck: bump Go and x/net | #183 (CHE-1426) | 30 | upstream/main server/go.mod has go 1.26.9 (commit 3bacc41f84) and golang.org/x/net v0.62.0 (commit ca593b3103); both above fork fix (go 1.26.9, x/net v0.60.0) | O12 |
| FX083 | PR check snapshot counts superseded cancelled check runs as failures | #374 (CHE-1994) | 39 | upstream 82492f94d6 "MUL-8510: fix(ghsnapshot): drop cancelled workflow runs that a newer run of the same workflow replaced from the PR rollup (#9860)"; snapshot.go fetchSupersededSuites (:224, doc :206-222) drops them, same root cause. Conditional: works only when the GitHub token has the optional Actions read permission (client.go tokenCanRead :217); without it nothing is dropped. Fork keeps its per-(name, app) started_at dedupe (latestIndexes). Lead may keep the row as present. | - |
| FX097 | Media reconciler claim query claims several rows per statement (nested-loop LIMIT 1 rescan) | #437 (CHE-2162, CHE-2171) | 24 | Upstream 16a067f30a 'MUL-8270: fix(db): keep lease claims within query limits (#9563)' rewrote ClaimNextChannelMediaPendingObjectForReconcile to `WITH due AS MATERIALIZED (... LIMIT 1 FOR UPDATE SKIP LOCKED)` (upstream/main channel.sql:1276-1315), same root cause. Fork's scalar-subquery variant came after; fork main already carries the upstream commit. The PR's decision_delivery_*_test.go edits are fork-only flake fixes. | - |
| FX186 | Issue detail showed "deleted" when the issue failed to load; it now shows a safe load error with a scoped retry | #901 (CHE-3411) | 67 | `8105995e72` MUL-8658 fix(issues): show safe load errors and scoped retry (#10001), 2026-11-06 12:52 ICT | none |

## Unknown

| ID | Fix | Fork PRs (CHE) | Size | Regression test | Depends on | Why unknown |
| --- | --- | --- | --- | --- | --- | --- |
| FX031 | Codex zero-tool false-negative CLI-access claims | #114 (CHE-775) | 112 | unverified | soft: D04 `codex.go` | Defect is runtime behavior (Codex claims no CLI access with zero tool calls); upstream codex.go has no equivalent detector but reproducing needs a live Codex run |
| FX032 | Codex model catalog and multi-agent v2 resume | #115, #119 (CHE-702, CHE-773, CHE-775, CHE-778) | 458 | unverified | hard: D04 `codex_model_catalog.go` | Upstream execenv/codex_multi_agent.go mitigates multi_agent/multi_agent_v2 but has no model-catalog rewrite; effect depends on Codex CLI runtime behavior and fork gpt-5.6/astra catalog |
| FX055 | AGY sessions with NOT_FOUND or malformed-call replay not retired | #249, #279 (CHE-1710, CHE-1715, CHE-1730, CHE-1812, CHE-1815) | 57 | unverified | soft: D05 `retirement uses fork poisoned-session store in daemon` | upstream/main server/pkg/agent/antigravity.go:~193 has the AGY backend, but no poisoned-session retirement path; whether upstream reproduces needs a live AGY run (fork fix relies on runtime-recorded executor errors) |
| FX077 | Tests-only: retry-pending gateway failure starts no coordinator run; failed run removes task temp dir | #366 (CHE-1126, CHE-1984) | 0 | n/a (tests-only change) | none | No production change (0 non-test lines), no defect. Behaviour already exists upstream: upstream/main:server/internal/service/delegated_failure_recovery_test.go:1097 TestFailTaskRetryPendingDoesNotWakeCoordinator, and daemon.go:8578-8582 execenv.RemoveTaskTempDir cleanup. Characterization tests, test-first waived by Cheese; at most an optional upstream test-only PR. |
| FX113 | Owner/admin can change a custom issue status category (with confirmation) | #511 (CHE-2148, CHE-2267, CHE-2274) | 366 | unverified | none | Not a defect fix but a capability change: upstream/main:server/internal/handler/issue_status.go:86-88 states UpdateIssueStatusRequest deliberately has no Category (immutable by design) and server/pkg/db/queries/issue_status.sql:74 'key and category are absent on purpose'. Reversing a deliberate upstream invariant is a product decision, so no bug state applies. Handler/UI/realtime code it patches exists upstream. |
| FX176 | Mermaid viewer iframe kept the old drawing under the new layout when its document changed; the iframe now remounts on a new document | #927 (CHE-3032) | 7 | yes: `mounts a fresh iframe when the document changes, so the old drawing cannot stay under the new layout` | none | Upstream `packages/views/editor/mermaid-viewer.tsx:281-284` renders `<iframe srcDoc={viewerDocument}>` with no key, but the PR records no real-browser repro, so the stale drawing is not shown to occur upstream |
| FX185 | A PR merge whose `closed` webhook was lost was never recorded; a closed-PR replay from the snapshot refresh now records the merge | #900 (CHE-3402) | 381 | yes: `TestGitHubReplayClosedPullRequestRecordsMerge` | none | Upstream `server/internal/handler/github.go:1321` records a merge only from the `closed` webhook and has no closed-PR replay, but the cause of the lost delivery is not established (the PR refuted the bot-sender theory), so upstream exposure is not shown |
| FX194 | Docs image build ran out of memory in the 4 GiB CI container; build workers are capped to the CPU quota, webpack footprint is trimmed and webpack replaces Turbopack | #942 (CHE-3165) | 2 | partial: `caps build workers to the CI container's CPU quota` (asserts config values, not memory) | none | Upstream `apps/docs/next.config.mjs` has no worker cap or webpack memory options, but the out-of-memory is in the fork's 4 GiB nested CI container; upstream CI exposure is not shown |
| FX207 | Stale DM detail not healed when the DM list shows a newer message | #975 (CHE-3600) | 177 | yes: `heals a mounted DM when the refreshed list has a newer message` | none | Upstream has no last_message_id in chat.sql or core, but the defect needs a dropped realtime event to reproduce; live run needed |
| FX212 | Codex runs lose the brief tail at the 32 KiB AGENTS.md cap | #1003 (CHE-3687) | 137 | yes: `TestCodexBriefFitsProjectDocLimit` | soft: D04 `codex` backend | Upstream has no project_doc_max_bytes handling; whether the upstream brief exceeds 32 KiB needs a live Codex run (fork brief is 56865 bytes, part of it fork-only content) |

## Stays in fork

Fork-only code; one line each. Grouped by the ledger row that owns the code (`other` = no single row).

**B01** — Run budget core: locked admission ledger, token reservation and settlement, per-workflow token activation

- FX110 Run budget miscounted finished issues and skipped the review gate; added per-issue raise (fork run budget). (#476 (CHE-1, CHE-1192, CHE-2152, CHE-2164, CHE-2226))
- FX154 Delayed exhaustion callback did not revalidate the budget window. (#734 (CHE-2807))
- FX156 Top-up/member move did not carry over or write the old window balance additively. (#737, #779 (CHE-2297, CHE-2805, CHE-2806, CHE-2885))
- FX167 Run-budget tests flaked: the 80% warning goroutine from `spendBudget` raced the edge-move test, and process-wide run-budget hooks leaked from earlier handlers; tests now clear and restore the hooks (test-only change). (#793 (CHE-3014), #956 (CHE-3579))
- FX223 CI backend-tests flaked on a shared claim runtime going stale, a run-budget hook race and an HTTP/2 deadline test; fixture runtime stays claimable and the budget hooks wait for started hooks. (#1059 (CHE-4008))

**B02** — Run budget notices, run-prompt budget line, snapshot endpoint, raise math and CLI get

- FX155 Snapshot, run prompt, raise preview and recovery command used a derived balance instead of the admitted ledger balance. (#735, #782 (CHE-2297, CHE-2806, CHE-2886))

**B03** — Member top-up endpoint and CLI, blocked-recovery reserve, server-origin exhaustion decision request

- FX159 Run-budget raise/top-up routes and workflow budget fields could be changed with agent/PAT credentials. (#745, #750 (CHE-2798, CHE-2824, CHE-2829))

**C01** — Description and long-thread disclosure, chronological reply/run slots and focus-safe fold controls

- FX007 Disclosure Show more/less control affordance polish (C01). (#41 (CHE-380, CHE-519))
- FX009 Fold All latch re-expanded active drafts/runs in disclosure UI (C01). (#49 (CHE-479))
- FX013 Show more control unmounted under focus-driven force-expand (C01). (#62 (CHE-554))

**C03** — Stage-completion dedupe, generation receipts and parent system comments

- FX010 Stage completion woke the parent more than once (C03 dedupe). (#55 (CHE-482, CHE-488))

**C06** — Task-bound decision intents, schema constraints and asked-record guards

- FX144 CLI collapsed 403 to generic copy, hiding task-scope refusals such as decision-intent errors (reverses upstream deliberate 403 policy). (#622 (CHE-2574, CHE-2591))
- FX214 Decision park lived in issue metadata and was clobbered by metadata writes; moved to its own table with a migration. (#1023 (CHE-3946))

**C07** — Decision records, human disclaim, veto and raise-only decision classes

- FX142 Recorded decision comments did not dispatch agent mentions. (#614 (CHE-2459, CHE-2463, CHE-2465))
- FX165 Decision records accepted non-reversible decision classes; the record route now refuses them and takes card-only routing. (#756 (CHE-2833))
- FX182 Agent-decision exclusion matched prose words; it now uses the typed Block-category only. (#882 (CHE-3364))

**C08** — Transactional decision delivery, inbox receipts, held groups and leased consequences

- FX096 Agent status guard allowed done while a decision request was unanswered; now keeps issue open (fork decision guard). (#436 (CHE-1527, CHE-2176))
- FX119 Waiting owners were not woken from recorded decision answers (fork decision delivery). (#536 (CHE-2199, CHE-2361))
- FX140 Wakeup run dropped the decision-card authorization of the originating run. (#608 (CHE-2488, CHE-2548))
- FX161 Decision wake policy was enforceable only in code; added DB trigger. (#757 (CHE-2798, CHE-2832))
- FX162 Answers were validated after delivery side effects instead of before. (#760 (CHE-2798, CHE-2831))

**C09** — Single-call decision requests, intent reuse and contextual cancellation

- FX065 DM runs lacked the open decision request their reply answers; decision ask posted twice (fork decisions). (#310, #330, #342 (CHE-1868, CHE-1907, CHE-1958, CHE-1890, CHE-1898, CHE-2028, CHE-1944))
- FX067 Decision cancellation lacked context and issue close opened several DMs (fork decisions). (#326 (CHE-1890, CHE-1897, CHE-1899))
- FX089 Decision request recipient/owner accepted only agent-style IDs; now takes workspace user and member IDs (fork decision requests). (#395 (CHE-2094))
- FX093 Closing an answered decision request sent a redundant DM note; suppressed (fork decision lifecycle). (#422 (CHE-2112, CHE-2114))

**C10** — Decision ping unread state and DM-list refresh

- FX049 DM unread logic missed continuation summaries and the first decision ping in a new DM. (#214, #241 (CHE-1608, CHE-1609, CHE-1662))
- FX051 DM list did not refresh on a decision ping (fork realtime sync). (#237 (CHE-1661, CHE-1667))

**C11** — DM continuation, saved compact prompts, slash command, announcements and pin preservation

- FX047 Fork DM compaction: /compact command missing, pins lost on compact, continuation announcement not shown. (#205, #207, #210 (CHE-1538, CHE-1582, CHE-1583, CHE-1584))
- FX112 Queued DM turns were not moved to the continuation DM (fork DM continuation). (#481 (CHE-2239))
- FX143 Fork CI red: low-contrast slash-command hint and missing zh-Hant keys for chat commands. (#616 (CHE-2080, CHE-2576))

**C12** — Automatic compaction settings, prompt-size reporting, context meter and manual compact action

- FX241 Context meter warned at 110k of a 1M window with a 200k threshold; warning now follows the compaction token threshold and the meter shows it. (#1175 (CHE-4418))

**C13** — Full-copy DM forks, optional first message, attachment transfer and fork-point links

- FX066 Fork-point links, plain titles and composer attachments not carried into forked DM (fork /fork). (#313, #314 (CHE-1873, CHE-1878, CHE-1872))

**C16** — Per-channel notification settings and coalesced DM inbox replies, off by default

- FX063 DM replies flooded the inbox; made opt-in (fork DM notifications). (#303 (CHE-1793, CHE-1848))

**C20** — DM annotations analogous to issue reply annotations

- FX104 DM annotations (C20) could not select text inside collapsed Earlier-DM rows or the Conversation summary (C32); fixed in the fork chat UI. Size mostly docs/ux captures. (#457 (CHE-2162, CHE-2171, CHE-2195))
- FX158 Annotations on closed decision cards stayed in the composer and blocked sending. (#742 (CHE-2823))

**C23** — Retired lane summaries and turn reminders

- FX048 Fork lane-summary chat feature (later retired): summaries/deleted decision intents mishandled; columns dropped. (#211, #236 (CHE-1539, CHE-1549, CHE-1587, CHE-1559, CHE-1610))

**C24** — Structured decision ask: schema decoder and lint, withdraw verb, source chip, stale-ask digest, human-mention recommendation

- FX111 Ask and withdrawal decision cards rendered as one in DM; now separate (fork decision cards). (#477 (CHE-1485, CHE-2088, CHE-2200, CHE-2234))
- FX181 Structured-ask lint refused asks over wording and counted agent text length in bytes; structural rules stay hard, wording checks are advisory, length counts characters. (#881 (CHE-3365))

**C25** — Decision quote cards, answer-on-card without a DM-agent run, answer receipts and picked-up-by-run display

- FX109 Recorded decision did not link back to its DM request card and answer message (fork decision quote cards). (#466 (CHE-2088, CHE-2225))
- FX146 Decision card answer with option and note rendered without label. (#641 (CHE-2627))
- FX171 Answering a product-direction decision card did not wake a completed asking task; card answers now wake it (behavior fix is #760 under FX162; this PR adds the repair migration 885 and coverage). (#804 (CHE-3025))
- FX192 Every issue run start refetched the DM list; refreshes are now coalesced and scoped to the matching DM. (#933 (CHE-3233))
- FX208 In-UI decision links were absolute from MULTICA_APP_URL and broke behind another origin; made relative. (#986 (CHE-3537))
- FX237 Tapping a decision option sent immediately and could not be cleared; tap now selects, Send posts, selection can be cleared. (#1160 (CHE-4390))
- FX246 A batch decision card left the per-issue cards it covered open; answering the batch now closes the covered cards. (#1196 (CHE-4453))

**C28** — Decision routing: one topic DM per root issue with steward, open-asks read, evening brief, answer change feed, project delegates, Markdown record

- FX108 Decision requests could land in a DM that never discussed the issue and re-asked without limit; now scoped plus once-a-day re-ask (fork decision routing). (#465 (CHE-1858, CHE-2205))

**C29** — Approve-to-execute: executable decision actions, action registry, issue targets, park issue while a decision is open

- FX189 Issues parked by a decision (`decision_park`) failed to open because non-primitive metadata broke the schema; non-primitive entries are now dropped and the issue loads. (#918 (CHE-3472))

**C32** — Predecessor DMs shown inline, collapsed and quotable

- FX153 Rolled-over DM opening showed uncleaned continuation block / predecessor preview. (#708 (CHE-2169, CHE-2628, CHE-2767))

**C34** — In-place compact: compaction epoch, epoch-bound resume, claim/compact serialisation

- FX101 A DM task started before an in-place compact could write its pre-compact session back as the DM resume pointer; writers now check the compaction epoch. (#453 (CHE-2169))
- FX150 Final reply of a run that rolled over to a continuation DM was not written into the new DM. (#683 (CHE-2119))
- FX180 Compact-turn rows in the DM list shared a React key (assistant messages with one task id), hiding the newest message; row keys are now unique. (#854 (CHE-3329))
- FX243 Auto-compaction summary always expanded, notice text wrong, open asks not repeated; summary folds by default and open asks repeat. (#1184 (CHE-4438))
- FX245 Compaction dropped recorded decisions of the DM; every recorded decision now carries into the next session. (#1189 (CHE-4447))

**C35** — Supplement steer for running DM tasks and follow-up delivery state

- FX234 A steered DM message was refused as a decision-record source; steered messages are now accepted. (#1145 (CHE-4316))

**C37** — Authenticated address-bar suggestions

- FX126 Search-suggestions query scanned beyond title/identifier in fork address-bar suggestions. (#551 (CHE-1590, CHE-2401, CHE-2402))

**C38** — Highlight own mentions and comments (web, mobile, @all) and jump to next mention

- FX095 Own-mention chip (C38) colour restyled twice: brand-inverted fill, then pastel-yellow tokens; mostly docs/ux evidence (PR 434 is 1080 lines, CSS ~4). (#434, #451 (CHE-2167, CHE-2193))
- FX151 Next-self-mention jump stopped settling on re-render/120 Hz displays. (#697 (CHE-2647, CHE-2704))
- FX188 The deferred chat content editor dropped `flagMentions` calls; the call is now forwarded to the live editor. (#911 (CHE-3436))

**C39** — Comment-ID chips with issue key, author and preview, and rich wakeup prompts with issue chips

- FX173 Mobile comment-chip taps scrolled to an estimated row offset and missed the target; the jump now re-scrolls until the target row is visible, resolves a reply to its parent row, and follows rows inserted above. (#821 (CHE-3116))

**C40** — Thread context on mention, resolve step and issue status summary

- FX098 Trigger thread context on long threads dropped the newest replies; now keeps newest (fork thread-context-on-mention feature). (#438 (CHE-2070))

**C41** — Plan revision compare and a new revision resolves the superseded revision's threads

- FX121 Fork-added plan-revision (PR 443) and DM annotation one-request keys had no zh-Hant translation, redding locales/parity.test.ts on main. (#540 (CHE-2124, CHE-2175, CHE-2375))
- FX152 supersession-links imported icons from wrong package. (#706 (CHE-2121, CHE-2766))

**C43** — Agent comment wakes are gated and agent comments that ask a human need mention or context

- FX183 The CLI refused agent comments by phrase match (`humanask`); the phrase refusal is removed and a member mention is no longer refused by text. (#883 (CHE-3366))

**C44** — Lazy Relations graph, CLI and fast native Relations report

- FX102 Fork relations graph had no edge-type filter and poor node/edge styling (fork Relations graph). (#454 (CHE-1, CHE-2085, CHE-2138, CHE-2194))
- FX107 Relations graph used wrong colours for custom statuses and omitted the non-parent edge legend (fork Relations graph). (#463 (CHE-2085, CHE-2194))
- FX135 Relations report hydrated all mentions up front; now loaded on demand. (#592 (CHE-2183, CHE-2488))

**C46** — Mermaid diagrams: container-width orientation setting, gesture zoom engine, on-brand theme

- FX175 Mermaid chat previews shrank text below body size and kept the old drawing after a redraw in the other direction; text now stays at body size inside a capped, scrolling frame and the viewer reloads on redraw. (#833 (CHE-2750))

**C49** — Web Push subscriptions, outbox and VAPID worker

- FX233 Passkey recovery left the user's Web Push subscriptions live; recovery now ends every subscription of that user only. (#1141 (CHE-3431))

**D01** — Atomic daemon single-instance startup and PID publication safety

- FX084 Desktop killed `multica daemon stop` after 15s while the fork CLI waited up to 35s for the daemon to release its instance lock, so restart never reached the start step. (#375 (CHE-1463, CHE-1522, CHE-1974))

**D02** — Fork daemon artifact distribution, server-hosted builds and install-while-busy

- FX190 Server-built daemon binaries had no `buildCommit` stamp; the build script now stamps it. (#926 (CHE-3539))

**D06** — Pre-LLM provider recovery, gateway backoff and retry-pending coordinator suppression

- FX054 Runtime kept dispatching to failing gateway and reused poisoned sessions (fork provider recovery). (#244 (CHE-1672, CHE-1699))
- FX248 Generic retry also fired for provider-failure recovery lineage; generic retry is now fenced off for that lineage. (#1209 (CHE-1773))

**D07** — Oversized leader issue sessions start fresh

- FX078 Leader runs kept resuming an ever-growing issue session; claim now drops prior_session_id when the last run's context_tokens >= 80k (fork leader/squad session handling). (#367 (CHE-1803, CHE-1948, CHE-1983))

**D19** — Idle started-issue guard with off, shadow or act mode, settable from Settings and the CLI

- FX216 Idle-guard receipt lived in issue metadata; moved to its own table. (#1025 (CHE-3947))

**D23** — Provider circuit breaker: per workspace and provider state, trip rule, claim gate, notice, state API, admin reset, audit, CLI and settings panel

- FX236 Provider breaker never recovered from half-open, tripped on single bursts and counted unknown failures; half-open admits exactly one canary, a burst counts as one signal, unknown never counts. (#1158 (CHE-4380))
- FX240 Triggers (squad leader enqueue, scheduled autopilot run) were refused or skipped during a breaker hold; now queued instead. (#1167, #1205 (CHE-4408, CHE-4460))

**G06** — Deterministic protocol lint, persisted violations, omission reporting and complete waiver lookup

- FX011 Protocol lint missed old waiver grants (truncated comment history) (G06). (#58, #70 (CHE-551, CHE-556))
- FX023 Protocol lint waiver claims matched too broadly (G06). (#93 (CHE-513, CHE-681))

**G08** — Typed duplicate admission for issue assignment, rerun lineage and autopilot-created issues

- FX005 Typed duplicate admission raced on first admission of a rerun lineage (G08). (#34 (CHE-485))
- FX012 Autopilot create_issue triggers skipped typed admission (G08); ~21-line autopilot.go ErrDuplicatePendingTask hunk is upstream-extractable. (#61 (CHE-486, CHE-561))

**G09** — Provider hold across dispatch, manual rerun, retry and claim

- FX242 Scheduled autopilot run was skipped for lack of success evidence; admitted on same-provider success evidence. (#1177 (CHE-4311))

**G10** — Jev flags, scoped client, state budget, production/no-key gate and missing-mention evaluation

- FX021 Jev evaluator accepted non-paired governance responses (G10). (#89 (CHE-684))

**G17** — Atomic case admission and budget reservation across windows, crash recovery and bounded ledger queries

- FX035 Run-budget/ledger queries unbounded; bounded and indexes verified (G17). (#140, #146 (CHE-729, CHE-874, CHE-884))

**G18** — Bounded observation, rollback deadlines, timeout classification and real-budget test controls

- FX039 Receipt classifier misclassified observation timeouts (G18). (#149 (CHE-937, CHE-945))
- FX041 Jev observation rollback not bounded/clamped to the shared deadline; tests could not lift the production observation budget (Jev governance). (#168, #295 (CHE-584, CHE-946, CHE-1578))

**G19** — Jev squad-leader routing, shadow mode, error handling and trigger shadow ledger

- FX056 Jev routing and governed-instruction CAS ignored DB read errors in comment trigger routing. (#251 (CHE-1711))

**G23** — Leader reconciliation proof and durable event-key precedence in briefing

- FX003 Squad leader could no_action substantive events without reconciling; reconciliation key precedence fixed (G23). (#11, #12 (CHE-329, CHE-346, CHE-348, CHE-359))

**G24** — Decision-bound agent writes on governed instruction fields and workflow routes, delegated-write setting, `--decision-id` dry run

- FX147 Governance write took its text from wrong field; now reads ask_json. (#667 (CHE-2666))

**H01** — Multi-organization GitHub installations with verified account authority

- FX127 Settings card hid Add account/organization and disconnected only the first installation (multi-installation UI from fork 208c62dbcb regressed). (#557 (CHE-2411))
- FX213 Approval settlement was not gated on the installation allowlist; unlisted installations are now denied. (#1006 (CHE-3700))
- FX247 Released Compose files did not pass GITHUB_ALLOWED_ACCOUNT_IDS to the server; now passed through. (#1199 (CHE-4450))

**H02** — Explicit PR close intent, quoted-code stripping and open-child completion gate

- FX008 Close-intent matcher fired on markdown code-span keywords and ignored open children (H02). (#42 (CHE-380, CHE-520))
- FX071 PR close-target checker accepted mismatched targets (fork PR workflow). (#344 (CHE-1, CHE-2, CHE-1911, CHE-1926, CHE-1930, CHE-1932))

**H03** — Merged PRs advance custom done-category review states

- FX125 Auto-complete moved the issue on merge even when the workflow step has a pr.merged edge (fork issueworkflow engine). (#549 (CHE-2398))

**H04** — Actor/task-aware automatic PR linking and CLI unlink repair

- FX123 Refs/Part-of body keywords (added by fork #373, CHE-1991) linked as delivery PRs and blocked/triggered merge auto-complete; new link_kind column. (#546 (CHE-2388))
- FX232 PRs from same-owner forks were not auto-linked; same-owner forks now link but do not move the issue. (#1140 (CHE-4300))

**H05** — Snapshot refresh for already-decided PRs and latest-run-per-name/app check filtering

- FX157 Stack child PRs and retargeted PRs kept a stale stored merge verdict after parent merge/base change. (#741 (CHE-1417, CHE-2796, CHE-2818))

**H06** — PR close-target validation and non-leaking sparse checkout

- FX073 Close-target checker sparse checkout misconfigured and leaked into reused X99 workspaces. (#351, #352, #355 (CHE-1884, CHE-1952, CHE-1676, CHE-1961))

**H07** — Durable merge-announcement writer, subsequently removed

- FX006 Merge announcement implied auto-pickup on no-close-intent merges (H07 writer). (#40 (CHE-511))

**H09** — Wake PR author agents on human reviews

- FX122 Wake of PR author agents missed resolver events (wakeup/github review path). (#545 (CHE-2298, CHE-2382))

**H10** — GitHub intake: signed delivery store, fork-PR denial, reconciler, route grants, quarantine and snapshot approval, kill switch, intake list

- FX114 Fork-PR denial rejected every same-repo PR in cheese-work/multica because GitHub sets fork=true on the repo itself, skipping auto-link, auto-complete and wakeups; plus startup repair sweep. (#517 (CHE-2301))
- FX117 GitHub unverified-head repair sweep touched rows never judged; scoped to judged rows (fork GitHub repair). (#527 (CHE-2210, CHE-2301))

**H11** — PR events captured, queued for the next turn and delivered to live runs; reviews bound to a PR head; merged/closed PR stops work

- FX132 pr.event receipts, conflict/checks-failed events and the legacy failed-check wake kept firing for merged/closed PRs. (#577, #602, #636 (CHE-2438, CHE-2529, CHE-2599))
- FX138 Fallback wake text for PR events had empty fields per event kind. (#605 (CHE-2298, CHE-2534))
- FX139 One PR event produced multiple wakes (notice in flight not covering legacy rule); coalesced to one. (#607, #637 (CHE-2540, CHE-2600))

**O02** — Web build CPU/worker OOM limits

- FX028 Fork CI web build OOM; cpus cap then webpack worker kept on (O02). (#107, #112 (CHE-702, CHE-736, CHE-745, CHE-768))

**O03** — Immutable image qualification and qualified C00 delivery admission

- FX014 CD could not resolve deployed commit when image lacked revision label; label now outranks tag guess (O03). (#68, #75 (CHE-530))
- FX017 C00 pulled candidate image from ghcr.io unauthenticated (O03). (#77 (CHE-530, CHE-549))
- FX018 CD admission treated absent path-filtered checks as failure; PyYAML missing for real filters (O03). (#78, #80 (CHE-530, CHE-651))
- FX026 CD admitted images before required checks completed (O03). (#100 (CHE-745))

**O04** — D2 migration quiescence and retained-archive rollback safety

- FX004 CD rollback used unverified preserved-archive inputs (D2 quiescence/rollback). (#25 (CHE-372))
- FX019 cutover.sh quiescence gate sequenced wrong and psql invoked wrongly (O04). (#82 (CHE-397, CHE-608, CHE-609, CHE-655))
- FX037 Quiescence CLI used 500ms statement budget instead of wall-clock timeout (O04). (#144 (CHE-372, CHE-823))

**O05** — Blue/green application slots, router state, atomic cutover and correct rollback tuple

- FX020 cutover-state.json recorded wrong image tuple after rollback (O05). (#85 (CHE-397, CHE-609, CHE-673, CHE-678))
- FX022 Cutover captured wrong baseline tuple, active colour and shared state root (O05). (#91, #95, #96 (CHE-702))
- FX024 D2 deploy bypassed A/B cutover (O05). (#97 (CHE-702))
- FX029 D2 deploy mis-quoted CUTOVER_DATABASE_URL (O05); PR mixes unrelated cutover changes. (#110 (CHE-685, CHE-738, CHE-765))
- FX030 A/B cutover had multiple port sources and could proceed before drain (O05). (#113 (CHE-773))
- FX033 Router state lived in compose-dir-derived path; now own C00 secret (O05). (#118 (CHE-701, CHE-768, CHE-773))
- FX124 C00 cutover script passed despite compose/env-key/image drift; drift-receipt check added. (#548 (CHE-1590, CHE-2395, CHE-2396))
- FX136 Cutover could proceed with mismatched frontend flag file; flagfile-check added. (#594 (CHE-2524))

**O06** — C00 queue stall alarms and superseded-candidate skipping

- FX034 C00 deployment queue stalls raised no alarm (O06). (#132 (CHE-783, CHE-823))
- FX087 CD queue superseded a candidate for any later main commit, including ones that cannot deploy; now only a deployable commit supersedes (fork CD pipeline). (#388 (no CHE key))

**O07** — Bounded candidate image-pull retry

- FX038 CD failed closed on a single candidate image pull error (O07). (#148 (CHE-823))

**O08** — Historical migration identity reconciliation

- FX025 Historical migration ledger rename broke CD reconciliation (O08). (#98 (CHE-374, CHE-548, CHE-736))
- FX046 Duplicate fork migration numbers (squad leader marker 614, pr_needs_attention 610, agent_task_queue_context 627) collided and broke migrate; renumbered. (#204, #221, #290 (CHE-1443, CHE-1547, CHE-1577, CHE-1417, CHE-1610, CHE-1727, CHE-1824))
- FX062 CD migration baseline check rejected legacy 627 name (fork CD). (#300 (CHE-1803, CHE-1809, CHE-1824, CHE-1843))

**O09** — Scoped Jev environment inputs survive backend recreation

- FX064 Fork CD lost scoped Jev inputs when recreating the backend (706 lines, mostly CD workflow). (#305 (CHE-1854))

**O10** — Integration tests fail without their database and avoid shared-fixture leakage

- FX053 Fork CI silently skipped integration/handler tests when the DB was missing. (#239, #350 (CHE-1667, CHE-1670, CHE-1676))
- FX168 Flaky backend tests blocked the merge queue: `daemon_install.sh` probe watchdog inherited the EXIT trap and deleted its workdir, Redis tests shared DB 14 and an ACL user name, a stored Default workflow leaked between tests, a wecom test asserted wall clock; each root cause fixed with no retries or skips (test files plus 4 lines of `daemon_install.sh`). (#811 (CHE-3029))
- FX211 Runtime usage tests depended on a shared runtime and flaked; pinned to the fixture runtime. (#994 (CHE-3635))
- FX221 CI test fixes: PostHog drop test double-close; run-budget hooks not restored after extra Handlers; fixture runtime past freshness window; drain tick lock timeout retry. Tests-only, no production change. (#1051 (CHE-2803))
- FX227 Handler test shards hit the default timeout under CI load; shards get a 20m configurable timeout. (#1105 (CHE-4243))
- FX229 TestDMProgress_AncestorWalkIsLinearInDepth flaked under CI load; deflaked. Tests-only. (#1115 (CHE-4266))

**O11** — Real-browser disclosure regressions and repaired upstream-sync E2E selectors/timeouts

- FX075 Fork upstream-sync E2E selectors/timeouts broke (fork e2e harness). (#359 (CHE-1900, CHE-1968))

**O13** — Isolated paired-replay instrumentation

- FX072 Perf instrumentation for paired replay (fork-only measurement tool). (#348 (CHE-1942))

**O14** — Fork build stamps and distinct server version/build/commit fields

- FX068 CD build stamps/version fields for C00 releases (fork release process). (#331 (CHE-1631, CHE-1900, CHE-1910))
- FX079 Fork build stamp (<upstream>-YYYYMMDD-hhmm) was missing in install-cli-from-ref.sh and the desktop-bundled CLI, so the daemon advertised the wrong version. (#368 (CHE-1910, CHE-1980))

**O15** — Fork changelog line contract, generation, preview and qualified release display

- FX128 Changelog fetch logged per-PR warnings as CD annotations instead of notes. (#566 (CHE-2183))
- FX163 Changelog generator accepted over-limit lines; decision banner rendered above the issue body. Mixes two unrelated changes. (#771 (CHE-2940))

**O16** — Autopilot cooldown lookup scope and unset probe wiring

- FX069 Schedule cooldown lookup ignored scoping and an empty provider recovery URL broke wiring (fork provider recovery). (#332 (CHE-1906, CHE-1911, CHE-1926))

**O17** — Fork CLI/skill reference and child completion briefing updates

- FX016 Fork runtime brief let sub-issues park at in_review instead of closing (O17). (#76 (CHE-517, CHE-605))

**O20** — Merge-group CI validation and reuse of green merge-group runs

- FX218 Queue ejection reported unknown while the merge_group run was still in progress; report now waits for the run. (#1037 (CHE-3991))

**O21** — CD approvals per version, superseded builds cancelled, approved deployments preserved, cached/parallel qualification, deploy flag isolation, backend shard

- FX141 C00 preflight failed on ambiguous short commit hash in server_commit. (#613 (CHE-2565))
- FX197 CD buildx steps used the default builder; each call is now bound to the named builder. (#954 (CHE-3594))
- FX210 CD builder left broken after a failed named builder setup and rate limits aborted the run; stale builders recovered and rate limits retried. (#991 (CHE-3633))

**O24** — Issue-link requirement on PRs and pre-merge security gate

- FX178 Security gate left merge-queue heads green after a PR verdict was revoked; queue heads now flip to failure on revocation. (#840 (CHE-3240))
- FX179 Security gate was not posted on merge-queue commits, blocking the queue; the workflow now posts it on merge-group commits. (#848 (CHE-3317))
- FX226 Security gate hid gh api stderr on failure; stderr now printed and the error still raised. (#1101 (CHE-4234))
- FX235 Security gate accepted a human test-first waiver record without one exact scope line; now fails closed. (#1146 (CHE-4326))

**O25** — Warning-budget normalisation fixes

- FX099 Fork CD/CI workflows emitted deprecation and runner warnings; fixed at root and a warning-budget gate added. (#445 (CHE-2183))
- FX116 Warning budget double-counted an eslint annotation filed under .github (fork warning-budget script). (#526 (CHE-2320))
- FX149 Fork CI warning-budget baseline failed instead of skipping runs whose jobs API returned 5xx. (#680 (CHE-2631, CHE-2700))
- FX224 execenv test logger emitted a stray WARN line that the warning budget counted; WARN silenced, ERROR kept. (#1090 (CHE-4202))

**O26** — Governance teardown flake fix, stale-verdict race test, web page-load baseline

- FX059 Test-only cleanup: chat-usage test stranded active tasks that flipped sweeper tests. (#278 (CHE-1527, CHE-1810))

**O27** — Fork documentation and process records: feature ledger, built-in skill CLI reference, changelog backfill and link, build-once rule, settings-first rule, active-agents run count

- FX015 Fork skill reference issues.md exceeded 500-line budget (O27 fork docs). (#74 (CHE-530, CHE-559, CHE-616))

**W02** — Per-issue workflow override and guarded reassignment

- FX085 Fork re-port bundle: Qoder run could move an issue to a status outside its (per-issue) workflow; workflow edit returned 409 before 403; flow-card admin gate and tooltip/label adoptions. (#382 (CHE-2004, CHE-2020))
- FX164 Status options in views followed the issue's project workflow, not the issue's own workflow override; options now resolve the issue's own workflow and hold single-issue writes while the override is unresolved. (#743 (CHE-2822))

**W03** — `--no-start` preserves a changed handler's workflow handoff

- FX249 `--no-start` help did not say a workflow handoff run is kept; help now documents it. (#1245 (CHE-4613))

**W04** — Green-check owner wakeup and optional workflow auto-advance

- FX184 `checks_finished` wakeup counted a pending security-gate status as finished; it now ignores the verdict context. (#893 (CHE-3389))

**W10** — Atomic recovery from PR-wakeup rate pause

- FX040 Platform PR wakeups lost rate-pause recovery atomicity (W10); touches shared wakeup queries but fix is fork wakeup feature. (#159 (CHE-1079, CHE-1082, CHE-1083, CHE-973))

**W12** — Wakeup activity identifies trigger, agent and run

- FX057 Wakeup activity rows showed no trigger/agent/run (fork wakeup feature). (#253 (CHE-1485, CHE-1726, CHE-1811, CHE-1819))

**W13** — Comment triggers fold into queued work, wake blocked leaders and avoid author self-trigger

- FX042 Comment author triggered its own run; fork comment-trigger fold added self-trigger guard. (#177 (CHE-1285))
- FX044 Comment triggers did not fold into queued run, blocked squad leaders not woken, blocker warnings missing (fork trigger/squad-leader flow). Mixed: 630 lines span 11 CHE items. (#185 (CHE-833, CHE-955, CHE-982, CHE-1048, CHE-1113, CHE-1117, CHE-1285, CHE-1418, CHE-1442, CHE-1443, CHE-1444))
- FX052 Test-only: integration expectations drifted after comment-fold change. (#238 (CHE-1418, CHE-1665, CHE-1667, CHE-1669))
- FX145 Member comment/status input on a blocked issue woke only squad leaders, not agent assignees. (#640 (CHE-2393))

**W15** — Per-step guarded edges, edge engine (entry, timeouts, child PRs), GitHub review guards and read-only flowchart view

- FX115 Step-done hint used the next board column and agents could move off a step along a non-exit; NextStatus now follows the first agent_done exit and CheckAgentMove refuses other moves (fork guarded-edges workflow). (#520 (CHE-2320))

**W30** — Stage queue WIP limits for review, device and browser gates

- FX220 Stage-queue dispatch refusals were reported as internal_error with no typed reason; refusals typed and every internal_error branch logged. (#1049 (CHE-4012))

**other**

- FX001 Verdict router failed open on malformed verdict shapes; now fails closed (fork verdict routing). (#4 (no CHE key))
- FX002 Workflow-project status resolution edge cases in fork workflow/status handling. (#5 (no CHE key))
- FX086 Parallel fork PRs reused migration numbers (678/679 chat, 689 decision_contract), failing TestMigrationNumericPrefixesAreUnique on main; renumbered to 687/688 and 690 (406 also fixes one test reference). (#386, #406 (CHE-2028, CHE-2069, CHE-2136))
- FX091 Agents could move an issue to a done-category status while a linked PR was open or a decision pending; guard added in fork status-guard code. (#415 (CHE-2148))
- FX130 staleVerdictRun only trusted cancel markers, so a verdict for an old head got through before the snapshot callback cancelled the review. (#572 (CHE-2437))
- FX160 Issue/project workflow rebinding could widen a run budget. (#753 (CHE-2798, CHE-2830))
- FX177 Issue search was slow on large workspaces; trigram GIN prefilter tables and triggers (migrations 893-895) cut latency. (#839 (CHE-3164))
- FX222 A cancelled mechanical-tier run became the latest result for a head; cancelled runs no longer count. (#1058 (CHE-3990))
- FX225 Telegram delivery lease tests slept and flaked; leases now expired explicitly. Tests-only. (#1100 (CHE-4224))
- FX228 workflow_run cascades burned the repo API budget; a finished CI run re-runs the classifier only when needed. (#1107 (CHE-4250))
- FX250 Reviewer runs started while admission checks were red or pending, wasting runs; runs deferred until admission is green (perf). (#1261 (CHE-4650))
