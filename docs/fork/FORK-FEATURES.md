# Fork feature ledger

This is the feature-level record for the 2026-10-23 upstream reconciliation.
It describes the fork; it does not authorize implementation, merging or deployment.

## Snapshot and provenance

- Fork `main`: `9ac3541dba50d3e1f06687779694b08095c21b79`.
- Upstream `v0.7.0`: `dbdbb299164347586e2650bb67684a490726a798`.
- Merge base: `654b8ca954034f8181eb6bbb4fc298dede760156`.
- Full-history comparison: 430 fork-only commits and 52 upstream-only commits.
- Primary sweep: `git log --first-parent --no-merges v0.7.0..9ac3541db`.
  This returns **245 commits after unshallowing**, not the initial shallow count
  of 226. Rows group those commits by feature rather than by commit.

The broader audit also checked 249 merged main-base PR records and 40 merged
stack-base PR records. Of their merge SHAs, 243 main-base and 24 stack-base
SHAs are ancestors of the fork pin. Squashing can retain a feature without
retaining a stack's merge SHA. Source at the pin is authoritative.
PRs #1 through #6 are not ancestors of the pin and are not shipped features
in this ledger. Earlier multi-installation and squad-briefing work is included
from the broader ancestry sweep. Stack references can also sit outside the
primary first-parent, non-merge filter.

`fork-only` means no behavior-equivalent upstream replacement was established.
`overlaps <commit/MUL id>` includes partial overlaps: the recommendation states
what the fork still adds. `upstreamed` requires confirmed upstream adoption;
no row is labelled upstreamed merely because a title looks similar.
Historical removals, verification support and design-only artifacts are labelled
explicitly. Their presence is not a claim that a proposed UI or runtime shipped.

R01–R20 refer to the comparison table on CHE-2006. The `Source:` pointers below were rechecked against fork `main`
`ffa2f97111694fa8c9421d29ff0a935d6b8ce49a` for CHE-2413 on 2026-10-29.
The snapshot, PR provenance and recommendations remain the 2026-10-23 record;
a keep recommendation is not proof that every entry point survived later syncs.
The issue holds the detailed paired-code evidence and the two product choices.
This is a static audit, not a runtime qualification of a reconciled tree.
The H01 restoration note was rechecked at main `f4960da0d9dcfad86ac2435d46b0bbc44dff3682`
after CHE-2411/#557 landed; the original issue table remains pinned to the earlier audit.

## Upstream sync preservation check

The `Fork sync preservation` CI check detects sync PRs by an `upstream-sync/`
head branch, a title containing `sync upstream` or `merge upstream` (including
`sync with upstream`), or a change to `UPSTREAM_VERSION`. This includes sync
PRs #138, #377 (CHE-2005), and #483 (CHE-2241), whose branches used `agent/`.
Other PRs pass without applying the preservation policy.

The check reads this ledger from the PR base, not from the proposed tree.
Deleted or renamed backtick-quoted `Source:` paths need an acknowledgement.
Directory pointers protect all descendant files; `*` matches within a filename.
An abbreviated filename after a full path uses the preceding path's directory.
Prose-only source descriptions are not machine-readable protection.

Fork-authored tests are files added or modified in the base tree relative to
the fork's last upstream tag, `v<base UPSTREAM_VERSION>`. The check recognizes
`.test.*`, `.spec.*`, `_test.*`, `-test.*`, and Python `test_*.py` filenames.
Deleting or renaming these tests needs an acknowledgement even without a
ledger entry. Missing history, the upstream tag, or parseable Source paths fails
the sync check rather than silently skipping protection.

For each protected old path, put a visible line in the PR body:

```text
Ported: packages/views/settings/components/old.tsx -> packages/views/settings/components/new.tsx
```

The new path must be a different file that exists at the exact PR head.
Backtick-quoted paths and Markdown list lines are accepted. Quoted, fenced,
and HTML-comment examples do not count. Body edits rerun the check.
An acknowledgement records the author's port claim; independent review must
still verify equivalent behavior and assertions. In-place content loss is
outside this deletion/rename check.

The workflow runs only trusted default-branch code on a GitHub-hosted runner.
The PR head is fetched as Git data, never checked out or executed. The new
workflow becomes active after it lands on the default branch; candidate
fixture tests also run in the existing CI scope job before that first merge.
The CHE-867 fixture replays the actual `github-tab.tsx` and
`github-tab.test.tsx` deletions from merge `2b558f361`. Its H01 row models the
corrected pre-sync UI inventory; the historical ledger did not list that UI.

## Collaboration and direct messages

| Feature | Area | Fork PRs | CHE issues | Upstream status | Recommendation |
| --- | --- | --- | --- | --- | --- |
| C01 Description and long-thread disclosure, chronological reply/run slots and focus-safe fold controls | Issue UI | #27, #28, #30, #31, #36, #41, #43, #49, #50, #62, #63 | CHE-431, CHE-434, CHE-435, CHE-436, CHE-463, CHE-476, CHE-479, CHE-502, CHE-519, CHE-554 | overlaps f896cc483 / MUL-8074; f1560daff | Merge both (R16). Upstream resolved-thread folding does not replace bounded content disclosure. Source: `packages/views/issues/components/issue-detail.tsx`, `packages/views/issues/components/description-disclosure.tsx`, `packages/views/issues/components/thread-disclosure.ts`, `packages/views/search/search-command.tsx`. |
| C02 Stale resolve rejection and serialized concurrent replies | Comments | #15, #35 | CHE-375, CHE-498 | overlaps 4b8873df3 | Keep ours (R17). Upstream edit revisions do not replace `known_as_of` on resolve. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/comment.go`. |
| C03 Stage-completion dedupe, generation receipts and parent system comments | Child issues | #55, #165 | CHE-488, CHE-1119 | overlaps 5ed09342a / MUL-7680 | Adopt the upstream engine, retaining receipt guarantees. Removing the visible legacy comments needs the R09 product choice. Source: `server/internal/handler/issue_child_done.go`, `server/migrations/509_stage_completion_wake.up.sql`. |
| C04 Deterministic status, ETA, active-run, CI and PR-head answers without an agent turn | Issue status | #56 | CHE-487 | fork-only | Keep ours. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/comment_status_query.go`, `server/internal/handler/comment.go`. |
| C05 Verified DM authentication provenance and durable channel origin | Chat trust | #188, #189 | CHE-1547, CHE-1551 | fork-only | Keep ours. Source: `server/cmd/server/router.go`, `server/internal/handler/chat.go`, `server/internal/handler/actor_guards.go`. |
| C06 Task-bound decision intents, schema constraints and asked-record guards | Decisions | #193, #195, #200, #209, #211, #233 | CHE-1548, CHE-1549, CHE-1558, CHE-1560, CHE-1586, CHE-1587 | fork-only | Keep the attested intent and source constraints. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/decision_intent.go`, `server/internal/handler/decision_request_api.go`. |
| C07 Decision records, human disclaim, veto and raise-only decision classes | Decisions | #234, #250, #256 | CHE-1628, CHE-1629, CHE-1630 | fork-only | Keep ours. Source: `server/cmd/multica/cmd_issue.go`, `server/cmd/server/router.go`, `server/internal/handler/decision_record_api.go`, `server/internal/handler/decision_disclaim.go`, `server/internal/handler/decision_veto.go`, `server/internal/handler/decision_class.go`. |
| C08 Transactional decision delivery, inbox receipts, held groups and leased consequences | Decisions | #203, #258, #260 | CHE-1570, CHE-1750, CHE-1753 | fork-only | Keep atomic delivery and consequence processing. Source: `server/cmd/multica/cmd_decision.go`, `server/internal/handler/decision_delivery.go`, `server/internal/handler/decision_worker.go`, `server/cmd/server/main.go`. |
| C09 Single-call decision requests, intent reuse and contextual cancellation | Decisions | #324, #326, #342 | CHE-1896, CHE-1899, CHE-1944 | fork-only | Keep one posted ask and issue-scoped cancellation. Source: `server/cmd/multica/cmd_decision.go`, `server/internal/handler/decision_request_api.go`, `server/internal/handler/decision_cancel_content.go`. |
| C10 Decision ping unread state and DM-list refresh | Chat inbox | #237, #241 | CHE-1661, CHE-1662 | fork-only | Keep first-ping unread and live refresh. Source: `packages/core/realtime/use-realtime-sync.ts`, `server/internal/handler/decision_request.go`. |
| C11 DM continuation, saved compact prompts, slash command, announcements and pin preservation | Chat compaction | #192, #205, #207, #210, #214, #243 | CHE-1582, CHE-1583, CHE-1584, CHE-1609, CHE-1612 | fork-only | Keep ours. Source: `packages/views/chat/chat-page.tsx`, `packages/views/chat/components/chat-input.tsx`, `packages/views/chat/components/chat-message-list.tsx`, `packages/views/settings/components/workspace-tab.tsx`, `server/cmd/multica/cmd_chat.go`, `server/cmd/multica/cmd_workspace.go`, `server/internal/handler/chat_continue.go`. |
| C12 Automatic compaction settings, prompt-size reporting, context meter and manual compact action | Chat context | #266, #281, #282, #285, #286 | CHE-1805, CHE-1806, CHE-1807, CHE-1808 | fork-only | Keep ours. Upstream has no matching chat usage/continue endpoints at the pin. Source: `packages/views/chat/components/chat-session-header.tsx`, `packages/views/chat/components/chat-context-meter.tsx`, `packages/views/settings/components/workspace-tab.tsx`, `server/internal/handler/chat_usage.go`, `server/internal/service/auto_compact.go`. |
| C13 Full-copy DM forks, optional first message, attachment transfer and fork-point links | Chat forks | #220, #227, #313, #314 | CHE-1606, CHE-1626, CHE-1872, CHE-1873 | fork-only | Keep source authorization, copied history and attachment ordering. Source: `packages/views/chat/components/chat-input.tsx`, `packages/views/chat/components/use-chat-fork.ts`, `packages/views/chat/components/chat-message-list.tsx`, `server/internal/handler/chat_fork.go`. |
| C14 DM rename without an agent turn | Chat commands | #213 | CHE-1607 | overlaps 51aa92412 | Merge both (R20). Reuse upstream title APIs, retaining `/rename`. Source: `packages/views/chat/chat-page.tsx`, `packages/views/chat/components/chat-input.tsx`, `packages/views/chat/components/use-chat-rename.ts`. |
| C15 DM search endpoint and anchored message navigation | Chat search | #273 | CHE-1792 | fork-only | Keep ours. Source: `server/cmd/server/router.go`, `server/internal/handler/chat_search.go`, `packages/views/chat/chat-page.tsx`. |
| C16 Per-channel notification settings and coalesced DM inbox replies, off by default | Notifications | #272, #276, #303 | CHE-1791, CHE-1793, CHE-1848 | fork-only | Keep the recorded opt-in default and coalescing. Source: `server/cmd/server/router.go`, `server/internal/handler/notification_channel.go`, `server/internal/handler/inbox.go`, `server/cmd/server/notification_listeners.go`. |
| C17 Web and desktop DM back-stack restoration | Chat navigation | #267 | CHE-1758 | fork-only | Keep ours. Source: `packages/views/chat/chat-page.tsx`, `packages/core/chat/store.ts`. |
| C18 Archived DMs are unpinned and cannot be re-pinned | Chat lifecycle | #275 | CHE-1801 | overlaps a51ab4d55 / MUL-4171 | Merge both (R20). Upstream archive/pin queries do not enforce archived-means-unpinned. Source: `server/internal/handler/chat.go`, `server/pkg/db/queries/chat.sql`, `server/migrations/653_unpin_archived_chat_sessions.up.sql`. |
| C19 DM message bubble timestamps in the client locale | Chat display | #311 | CHE-1871 | fork-only | Keep the fork display behavior. Source: `packages/views/chat/components/chat-message-list.tsx`, `packages/views/chat/components/message-time.ts`. |
| C20 DM annotations analogous to issue reply annotations | Chat interaction | #312 | CHE-1876 | fork-only | Keep ours. Upstream issue annotations do not provide the DM surface. Source: `packages/views/chat/chat-page.tsx`, `packages/views/chat/components/chat-input.tsx`, `packages/views/chat/components/chat-annotations.tsx`, `packages/views/chat/components/use-chat-annotations.tsx`. |
| C21 Failed issue-mention hydration recovers on focus | Chat rendering | #199 | CHE-1569 | fork-only | Keep the recovery. Source: `packages/views/issues/components/issue-chip.tsx`. |
| C22 Workspace site-search URLs, Cmd+K and OpenSearch discovery | Workspace search | #212, #219 | CHE-1591, CHE-1592 | fork-only | Keep ours. Source: `apps/web/components/workspace-search-command.tsx`, `apps/web/components/site-search-tab.tsx`, `apps/web/app/[workspaceSlug]/(dashboard)/search/page.tsx`, `apps/web/app/[workspaceSlug]/opensearch.xml/route.ts`. |
| C23 Retired lane summaries and turn reminders | Chat history, removed | #191, #211, #215, #236 | CHE-1552, CHE-1587, CHE-1610 | fork-only | Keep removed. #215 reverts the behavior and #236 removes its storage. Do not resurrect the experiment during sync. Source: `server/internal/handler/chat_lane_summary_removed_test.go`, `server/cmd/migrate/migrate_remove_lane_summary_test.go`. Removal evidence only; the retired behavior is not a retained runtime entry point. |

## Workflow, wakeup and GitHub coordination

| Feature | Area | Fork PRs | CHE issues | Upstream status | Recommendation |
| --- | --- | --- | --- | --- | --- |
| W01 CLI read/edit of project workflows | Workflow CLI | #364 | CHE-1982 | overlaps 0beaeddc0 / MUL-8120; da1671ca2 / MUL-7420 | Merge both (R03). Retain the CLI and apply upstream owner checks. Source: `server/cmd/multica/main.go`, `server/cmd/multica/cmd_workflow.go`. |
| W02 Per-issue workflow override and guarded reassignment | Workflows | #370 | CHE-1986 | overlaps da1671ca2 / MUL-7420 | Keep the override extension (R03). Upstream project workflows do not supply this endpoint. Source: `packages/views/workflows/issue-flow-card.tsx`, `packages/views/issues/components/issue-detail.tsx`, `server/cmd/multica/cmd_issue.go`, `server/internal/handler/issue_workflow_override.go`. |
| W03 `--no-start` preserves a changed handler's workflow handoff | Workflow execution | #369 | CHE-1988 | overlaps da1671ca2 / MUL-7420 | Keep the fork handoff correction. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/issue_workflow_entry.go`. |
| W04 Green-check owner wakeup and optional workflow auto-advance | Workflow CI | #365, #372 | CHE-1966, CHE-1992 | overlaps e720fe221 / MUL-8135 | Keep ours (R02). Upstream checks-finished includes failures and does not auto-advance the fork step. Source: `packages/views/workflows/handoff-form.tsx`, `server/internal/handler/github.go`, `server/internal/handler/issue_workflow_ci_advance.go`, `server/internal/service/issue_wakeup_system.go`. |
| W05 Automatic PR merge/failure/attention wakeups, including base-branch and accepted-to-draft changes | PR wakeups | #157, #184, #230 | CHE-1417, CHE-1665 | overlaps e720fe221 / MUL-8135 | Merge both (R08). Upstream explicit conflict watches do not replace automatic defaults. Source: `server/internal/handler/github.go`, `server/internal/service/issue_wakeup_system.go`, `server/internal/integrations/ghsnapshot/snapshot.go`. |
| W06 Built-in definition model, scoped cascade, CRUD/preview and legacy projection/backfill | Wakeup configuration | #160, #162, #163, #166 | CHE-1082 | overlaps 5ed09342a / MUL-7680 | Merge both (R10). Keep fork configuration semantics around the shared wakeup engine. Source: `server/cmd/server/router.go`, `server/internal/handler/wakeup_definition.go`, `server/internal/service/wakeup_definition_resolve.go`, `server/internal/service/wakeup_builtin_exec.go`. |
| W07 Separate local/default wakeup capacity pools | Wakeup admission | #169 | CHE-1193 | fork-only | Keep the pool separation. Source: `server/internal/handler/issue_wakeup.go`, `server/internal/service/wakeup_default_capacity.go`. |
| W08 Aggregate wakeup admission budgets | Wakeup admission | #170 | CHE-1194 | fork-only | Keep aggregate limits. Source: `server/internal/service/issue_wakeup.go`, `server/internal/service/wakeup_aggregate.go`, `server/internal/service/wakeup_builtin_exec.go`. |
| W09 Explicit suppress/defer when an agent already has active work | Wakeup admission | #171 | CHE-1195 | fork-only | Keep explicit dispositions. Source: `server/internal/service/issue_wakeup_join.go`, `server/internal/service/wakeup_active_run.go`, `server/internal/service/wakeup_builtin_exec.go`. |
| W10 Atomic recovery from PR-wakeup rate pause | Wakeup reliability | #159 | CHE-1083 | overlaps 5ed09342a / MUL-7680 | Retain the atomic fork recovery when combining engines. Source: `server/internal/service/issue_wakeup_system.go`. |
| W11 Workspace-wide wakeup firing pause | Wakeup settings | #254, #255 | CHE-1727 | fork-only | Keep the backend switch. #255 contains design directions, not a claim that every proposed UI shipped. Source: `packages/views/settings/components/wakeups-tab.tsx`, `packages/views/issues/components/wakeups-paused-banner.tsx`, `server/cmd/multica/cmd_issue_wakeup.go`, `server/internal/handler/workspace_wakeups_pause.go`. |
| W12 Wakeup activity identifies trigger, agent and run | Wakeup activity | #253 | CHE-1726 | overlaps 5ed09342a / MUL-7680 | Merge both without losing fork attribution (R06). Source: `packages/views/issues/components/issue-detail.tsx`, `packages/views/issues/components/wakeup-activity.tsx`, `server/internal/service/issue_wakeup_activity.go`. |
| W13 Comment triggers fold into queued work, wake blocked leaders and avoid author self-trigger | Trigger admission | #177, #185 | CHE-1285, CHE-1418 | fork-only | Keep dedupe and self-trigger protection. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/comment.go`, `server/cmd/multica/comment_self_mention.go`. |
| W14 Idle squad-leader failure markers | Squad recovery | #186 | CHE-1443 | fork-only | Keep durable failure visibility. Source: `server/internal/service/task.go`, `server/migrations/614_squad_leader_failure_marker.up.sql`. |
| H01 Multi-organization GitHub installations with verified account authority | GitHub integration | #13, #19 | CHE-268 | overlaps caeb146ba | Merge both (R15). Do not replace OAuth/account-authority verification with an installation ID alone. Source: `packages/views/settings/components/code-tab.tsx`, `packages/views/settings/components/repositories-section.tsx`, `server/cmd/server/router.go`, `server/internal/handler/github.go`. The audit at `ffa2f9711` found the add-organization and per-organization disconnect UI and its original tests lost in `2b558f361`; CHE-2411/#557 restored these controls and regression coverage on current main. Tests: `packages/views/settings/components/code-tab.test.tsx`, `packages/views/settings/components/repositories-section.test.tsx`. |
| H02 Explicit PR close intent, quoted-code stripping and open-child completion gate | PR lifecycle | #40, #42 | CHE-511, CHE-520 | overlaps 0da0b46bb / MUL-7672 | Merge both (R11). Retain fork code-span and direct-child safeguards. Source: `.github/PULL_REQUEST_TEMPLATE.md`, `server/internal/handler/github.go`, `server/internal/handler/pr_auto_complete.go`. |
| H03 Merged PRs advance custom done-category review states | PR lifecycle | #371 | CHE-1990 | overlaps fc4a61570 / MUL-7726 | Keep ours (R12). Only literal `done` and `cancelled` suppress the fork transition. Source: `server/internal/handler/pr_auto_complete.go`, `server/internal/handler/github.go`. |
| H04 Actor/task-aware automatic PR linking and CLI unlink repair | PR linking | #339, #373 | CHE-1933, CHE-1991 | overlaps 0da0b46bb / MUL-7672 | Merge both (R13). Keep fork policy, CLI repair and transactional exclusion handling. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/github.go`. |
| H05 Snapshot refresh for already-decided PRs and latest-run-per-name/app check filtering | PR snapshots | #230, #374 | CHE-1665, CHE-1994 | overlaps e720fe221 / MUL-8135; ecce58986 / MUL-5265 | Merge both (R14). Retain automatic refresh and superseded-check filtering. Source: `server/internal/handler/github.go`, `server/internal/integrations/ghsnapshot/snapshot.go`. |
| H06 PR close-target validation and non-leaking sparse checkout | Delivery CI | #344, #351, #352, #355 | CHE-1676, CHE-1932, CHE-1952, CHE-1961 | fork-only | Keep the close-target gate and workspace cleanup fixes. Source: `.github/workflows/pr-close-target-check.yml`, `scripts/pr-close-target-check.mjs`, `.github/workflows/ci.yml`. |
| H07 Durable merge-announcement writer, subsequently removed | GitHub history, removed | #17, #22, #24, #154 | CHE-374, CHE-384 | fork-only | Keep removed. #154 already removes the runtime writer. Old migrations or tests do not justify restoring the feature. Source: `server/internal/handler/github.go`, `server/migrations/576_drop_github_merge_announcement_table.up.sql`. Removal evidence only; the retired behavior is not a retained runtime entry point. |
| H08 CLI browser handoff exchanged for a CLI token | Authentication | #206 | CHE-1553 | overlaps da278c7d4 / MUL-8154 | Keep the fork handoff exchange and add upstream machine-credential mint guards. Source: `packages/views/auth/login-page.tsx`, `apps/web/app/auth/callback/page.tsx`, `server/internal/handler/auth.go`. |

## Governance, Jev and evidence

| Feature | Area | Fork PRs | CHE issues | Upstream status | Recommendation |
| --- | --- | --- | --- | --- | --- |
| G01 Field-scoped agent denial for governed instruction writes and the Hermes exception | Instruction governance | #29, #108 | CHE-455, CHE-764 | fork-only | Keep the trust boundary. Source: `server/internal/handler/agent.go`, `server/internal/handler/squad.go`, `server/internal/handler/workspace.go`, `server/internal/handler/governed_instruction_fields.go`, `server/internal/handler/governed_instruction_hermes_exception.go`. |
| G02 Owner-only digest compare-and-swap for workspace/squad/agent instructions | Instruction writes | #120, #178 | CHE-789, CHE-1300 | fork-only | Keep guarded writes and ambiguous-outcome handling. Source: `server/cmd/multica/cmd_agent.go`, `server/cmd/multica/cmd_squad.go`, `server/cmd/multica/cmd_workspace.go`, `server/internal/handler/governed_instruction_owner_cas.go`. |
| G03 Lossless file/stdin squad instruction input | CLI safety | #94 | CHE-727 | fork-only | Keep byte-preserving input. Source: `server/cmd/multica/cmd_squad.go`. |
| G04 Two-agent governed-write votes and CLI vote receipts | Instruction governance | #309, #356 | CHE-1862, CHE-1936 | fork-only | Keep independent vote requirements. Source: `server/cmd/multica/governed_cas.go`, `server/cmd/multica/cmd_agent.go`, `server/cmd/multica/cmd_squad.go`, `server/cmd/multica/cmd_workspace.go`, `server/internal/handler/governed_instruction_vote.go`. |
| G05 Required source-link enforcement on the server | Result provenance | #67 | CHE-408 | fork-only | Keep server enforcement. Runtime advice alone is not equivalent. Source: `server/cmd/multica/local_path_link_lint.go`, `server/internal/handler/comment.go`, `server/internal/handler/issue.go`, `server/internal/handler/required_source_link.go`. |
| G06 Deterministic protocol lint, persisted violations, omission reporting and complete waiver lookup | Protocol checks | #46, #57, #58, #70, #93 | CHE-529, CHE-551, CHE-552, CHE-556, CHE-681 | fork-only | Keep enforcement and narrow waiver interpretation. Source: `server/cmd/protocol_lint_report/main.go`, `server/internal/handler/daemon.go`, `server/internal/handler/protocol_lint_report.go`, `server/pkg/protocollint/protocollint.go`. |
| G07 Checkpoint-based briefing, reduction and claim-time persistence | Briefing context | #54, #64 | CHE-489, CHE-593 | fork-only | Keep checkpoints and claim persistence. Source: `server/internal/handler/daemon.go`, `server/internal/handler/checkpoint_claim.go`, `server/internal/checkpoint/checkpoint.go`, `server/internal/daemon/prompt.go`. |
| G08 Typed duplicate admission for issue assignment, rerun lineage and autopilot-created issues | Admission | #33, #34, #39, #45, #48, #51, #52, #61 | CHE-485, CHE-486, CHE-524, CHE-525, CHE-526, CHE-527, CHE-561 | fork-only | Keep durable duplicate handling and first-admission race protection. Source: `server/cmd/multica/cmd_issue.go`, `server/internal/handler/issue_trigger.go`, `server/internal/handler/task_lifecycle.go`, `server/internal/service/task.go`, `server/internal/service/autopilot.go`. |
| G09 Provider hold across dispatch, manual rerun, retry and claim | Provider policy | #65, #71, #84 | CHE-588, CHE-607, CHE-675 | fork-only | Keep the policy gate, not an HTTP-503 substitute. Source: `server/internal/handler/task_lifecycle.go`, `server/internal/handler/admission.go`, `server/internal/service/provider_hold.go`, `server/internal/service/task.go`. |
| G10 Jev flags, scoped client, state budget, production/no-key gate and missing-mention evaluation | Jev integration | #79, #86, #87, #89 | CHE-621, CHE-683, CHE-684 | fork-only | Keep default-off targeting and strict response pairs. Source: `server/cmd/server/jev.go`, `server/cmd/govreplay/main.go`, `server/internal/featureflags/jev.go`, `server/internal/governance/evaluator.go`. |
| G11 Jev routing receipts independent of published comments | Governance evidence | #99 | CHE-685 | fork-only | Keep durable receipts. Source: `server/internal/handler/comment.go`, `server/internal/handler/comment_governance_receipt.go`, `server/internal/governance/receipt/receipt.go`. |
| G12 Canonical inherited rule revision resolution | Governance rules | #121 | CHE-688 | fork-only | Keep canonical inheritance. Source: `server/internal/governance/rules.go`. |
| G13 Workspace governance control gate and proposal task credential restriction | Governance execution | #126, #130, #141 | CHE-707, CHE-709 | fork-only | Keep control, evaluation budget and credential boundaries. Source: `server/internal/handler/governance_config.go`, `server/internal/handler/governance_proposal.go`, `server/internal/handler/jev_credential.go`, `server/internal/governance/control.go`. |
| G14 Immutable cases, fenced lifecycle transitions and expired-evidence refresh | Governance cases | #122, #124, #158 | CHE-704, CHE-706, CHE-730 | fork-only | Keep immutable identity and fenced transitions. Source: `server/internal/handler/governance_case_audit.go`, `server/internal/governance/caselifecycle/service.go`, `server/internal/governance/caselifecycle/refresh.go`. |
| G15 Case evidence redaction, retention and audited provenance export | Evidence privacy | #104, #123 | CHE-705, CHE-755 | fork-only | Keep audited, read-only export and redaction. Source: `server/cmd/multica/cmd_provenance.go`, `server/internal/handler/provenance.go`, `server/internal/handler/governance_case_audit.go`, `server/internal/service/provenance_export.go`. |
| G16 Export privacy modes, retention and admin controls | Privacy settings | #105, #109 | CHE-766, CHE-771 | fork-only | Keep settings and retention controls. Source: `packages/views/settings/components/settings-page.tsx`, `packages/views/settings/components/export-privacy-tab.tsx`, `server/internal/handler/workspace_export_privacy.go`. |
| G17 Atomic case admission and budget reservation across windows, crash recovery and bounded ledger queries | Governance budgets | #128, #135, #136, #140, #142, #145, #146 | CHE-780, CHE-863, CHE-864, CHE-870, CHE-883, CHE-884 | fork-only | Keep reservation/cap invariants and recovery evidence. Source: `server/internal/governance/caselifecycle/admission.go`, `server/internal/governance/admission.go`, `server/internal/governance/budget.go`, `server/internal/governance/budget_accounting.go`. |
| G18 Bounded observation, rollback deadlines, timeout classification and real-budget test controls | Observation receipts | #149, #150, #168, #295 | CHE-920, CHE-937, CHE-946, CHE-1578 | fork-only | Keep production bounds and explicit test-budget overrides. Source: `server/internal/handler/comment_governance_receipt.go`, `server/internal/governance/receipt/receipt.go`. |
| G19 Jev squad-leader routing, shadow mode, error handling and trigger shadow ledger | Squad routing | #245, #251, #257, #334 | CHE-1711, CHE-1745, CHE-1918 | fork-only | Keep routing and log-only evidence separate from execution. Source: `server/internal/handler/squad.go`, `server/internal/handler/comment.go`, `server/internal/handler/squad_jev_routing.go`, `server/internal/handler/squad_jev_shadow.go`, `server/cmd/run_gate_report/main.go`. |
| G20 Jev mode/confidence, credential, rollback and API-URL controls | Settings | #268, #299, #306, #318 | CHE-1839, CHE-1853, CHE-1881 | fork-only | Keep implemented controls. #268 also contains design artifacts. Source: `packages/views/settings/components/settings-page.tsx`, `packages/views/settings/components/jev-governance-tab.tsx`, `packages/views/settings/components/jev-api-url-row.tsx`, `packages/views/settings/components/jev-credential-pane.tsx`, `packages/views/settings/components/jev-history-pane.tsx`, `server/internal/handler/governance_config.go`. |
| G21 Owner maintenance and conditional agent environment controls | Runtime administration | #156 | CHE-1039 | fork-only | Keep scoped owner authorization. Source: `server/cmd/multica/cmd_daemon_maintenance.go`, `server/cmd/multica/cmd_agent.go`, `server/internal/handler/agent_env_patch.go`, `server/internal/daemon/maintenance.go`. |
| G22 Credential-exclusive quota-runner execution | Execution admission | #175 | CHE-1279 | fork-only | Keep credential isolation and quota admission. Source: `server/internal/handler/daemon.go`, `server/internal/daemon/daemon.go`, `server/pkg/credentialexec/boundary.go`. |
| G23 Leader reconciliation proof and durable event-key precedence in briefing | Squad briefing | #11, #12, #76 | CHE-348, CHE-359, CHE-605 | fork-only | Keep reconciliation evidence and the corrected child completion guidance. Source: `server/internal/handler/squad_briefing.go`, `server/internal/daemon/execenv/runtime_config_sections.go`. |

## Daemon and runtime extensions

| Feature | Area | Fork PRs | CHE issues | Upstream status | Recommendation |
| --- | --- | --- | --- | --- | --- |
| D01 Atomic daemon single-instance startup and PID publication safety | Daemon lifecycle | #187 | CHE-1463 | overlaps 191c782d6 / MUL-7988 | Merge both (R18). Consolidate profile locking without dropping direct `Daemon.Run` protection. Source: `server/cmd/multica/cmd_daemon.go`, `server/internal/daemon/daemon.go`, `server/internal/daemon/instance_lock.go`. |
| D02 Fork daemon artifact distribution, server-hosted builds and install-while-busy | Daemon updates | #155, #361, #362, #363 | CHE-1012, CHE-1979, CHE-1980 | overlaps fcd13aece / MUL-2100 | Keep ours (R19). Installation can occur while busy, but restart still waits for the idle barrier. Source: `server/cmd/multica/cmd_update.go`, `server/cmd/server/router.go`, `server/internal/handler/daemon_update.go`, `server/internal/daemon/auto_update.go`, `scripts/build-daemon-binaries.sh`. |
| D03 Per-agent Hermes external-memory opt-in | Hermes environment | #151 | CHE-888 | fork-only | Keep explicit opt-in. Upstream's memory disabling is not the opt-in extension. Source: `server/internal/daemon/execenv/hermes_home.go`. |
| D04 Codex SQLite-init retry, false-negative tool-access handling, forced model catalog and poisoned multi-agent resume avoidance | Codex runtime | #101, #114, #115, #119 | CHE-702, CHE-737, CHE-775, CHE-778 | fork-only | Keep the narrowly witnessed recovery and model/tool configuration. Source: `server/pkg/agent/codex.go`, `server/pkg/agent/codex_resume_multi_agent.go`, `server/internal/daemon/execenv/codex_home.go`, `server/internal/daemon/execenv/codex_model_catalog.go`. |
| D05 AGY managed MCP config and retirement of poisoned executor/malformed-call sessions | Runtime recovery | #249, #279, #287 | CHE-1715, CHE-1815, CHE-1821 | fork-only | Keep scoped MCP propagation and fresh-session recovery. Source: `server/internal/daemon/daemon.go`, `server/internal/daemon/poisoned.go`, `server/internal/daemon/execenv/antigravity_mcp.go`. |
| D06 Pre-LLM provider recovery, gateway backoff and retry-pending coordinator suppression | Provider recovery | #174, #244, #366 | CHE-1239, CHE-1699, CHE-1984 | overlaps 53c0088f1 / MUL-7847; ebc7c4696 | Merge both (R04/R05). Take terminal notices and alphanumeric status boundaries without deleting fork recovery. #366 adds tests, not production logic. Source: `server/cmd/server/router.go`, `server/internal/service/provider_failure_guard.go`, `server/internal/service/task.go`, `server/pkg/taskfailure/classify.go`. |
| D07 Oversized leader issue sessions start fresh | Context recovery | #367 | CHE-1983 | fork-only | Keep bounded session reuse. Source: `server/internal/handler/daemon.go`, `server/internal/handler/daemon_leader_session.go`. |
| D08 Ignore repeat delivery of an already executing task | Delivery dedupe | #358 | CHE-1972 | fork-only | Keep the execution fence. Source: `server/internal/daemon/daemon.go`, `server/internal/daemon/duplicate_delivery.go`. |
| D09 Claimed-model readback alongside claim provenance | Task inspection | #357 | CHE-1962 | overlaps 482ec69c1 / MUL-7969 | Merge both (R01). Upstream outcome metrics do not replace the claimed model field. Source: `server/cmd/multica/cmd_agent.go`, `server/internal/handler/agent.go`, `server/internal/handler/daemon.go`. |
| D10 Agent-skill runtime target setting from the CLI | Skill administration | #349 | CHE-1884 | fork-only | Keep the runtime-set command. Source: `server/cmd/multica/cmd_agent_runtime_skills.go`. |
| D11 Price model-specific Hermes/Codex usage | Usage accounting | #143 | CHE-881 | fork-only | Keep fork model prices until an exact upstream price/model mapping is confirmed. Source: `packages/views/runtimes/utils.ts`, `server/internal/metrics/pricing.go`. |
| D12 Bounded WeCom inbox acceptance wait | IM reliability | #298 | CHE-1842 | fork-only | Keep deadline termination. Source: `server/internal/integrations/wecom/relay_outbound.go`. |

## Delivery, infrastructure and verification support

| Feature | Area | Fork PRs | CHE issues | Upstream status | Recommendation |
| --- | --- | --- | --- | --- | --- |
| O01 X99 build/CI ownership, runner migration and shared-Postgres/turbo contention limits | CI routing | #20, #44, #73, #102, #103, #106, #182, #333 | CHE-392, CHE-522, CHE-701, CHE-731, CHE-748, CHE-1381, CHE-1922 | fork-only | Keep host ownership and isolation. Later runner replacement supersedes the older Blacksmith migration, not the X99 invariant. Source: `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `.github/workflows/cd-qualification.yml`, `scripts/test-go.sh`. |
| O02 Web build CPU/worker OOM limits | Build resources | #107, #112 | CHE-702, CHE-768 | fork-only | Keep bounded build concurrency. Source: `apps/web/next.config.ts`. |
| O03 Immutable image qualification and qualified C00 delivery admission | Continuous delivery | #21, #23, #47, #68, #75, #77, #78, #80, #100, #360 | CHE-372, CHE-530, CHE-549, CHE-651, CHE-745 | fork-only | Keep immutable provenance, real check/path-filter evaluation and registry access. Source: `.github/workflows/cd-qualification.yml`, `.github/workflows/cd-deploy.yml`, `deploy/cd/admission.mjs`, `deploy/cd/deploy.sh`. |
| O04 D2 migration quiescence and retained-archive rollback safety | Database delivery | #25, #32, #82, #97, #110, #144 | CHE-372, CHE-397, CHE-765, CHE-823 | fork-only | Keep drain-before-gate sequencing and bounded wall-clock behavior. Source: `deploy/cd/cutover.sh`, `deploy/cd/quiescence.mjs`, `deploy/cd/migrate-supervised.sh`, `deploy/cd/render-cutover-remote-script.sh`. |
| O05 Blue/green application slots, router state, atomic cutover and correct rollback tuple | Application delivery | #66, #85, #91, #95, #96, #113, #118 | CHE-397, CHE-678, CHE-768, CHE-773 | fork-only | Keep independent state roots and exact artifact tuple tracking. Source: `deploy/cd/cutover.sh`, `deploy/cd/router.sh`, `deploy/cd/capture-tuple.sh`, `deploy/cd/docker-compose.ab.yml`. |
| O06 C00 queue stall alarms and superseded-candidate skipping | Delivery queues | #132, #376 | CHE-823, CHE-1995 | fork-only | Keep alarms and skip old candidates before approval. Source: `.github/workflows/cd-queue-watch.yml`, `.github/workflows/cd-deploy.yml`, `deploy/cd/check-deploy-queue.py`, `deploy/cd/candidate-preflight.sh`. |
| O07 Bounded candidate image-pull retry | Delivery reliability | #148 | CHE-823 | fork-only | Keep bounded retry followed by fail-closed admission. Source: `deploy/cd/cutover.sh`. |
| O08 Historical migration identity reconciliation | Database compatibility | #98, #204, #221, #290, #300 | CHE-736, CHE-1577, CHE-1610, CHE-1824, CHE-1843 | fork-only | Keep committed identities and compatibility records. Renumbering on the new sync is separate work. Source: `server/migrations/614_squad_leader_failure_marker.up.sql`, `server/migrations/622_pr_needs_attention_wakeup.up.sql`, `server/migrations/630_agent_task_queue_context.up.sql`, `server/migrations/653_unpin_archived_chat_sessions.up.sql`, `deploy/cd/release-packet.mjs`. |
| O09 Scoped Jev environment inputs survive backend recreation | Delivery configuration | #305 | CHE-1854 | fork-only | Keep scoped runtime inputs. Source: `deploy/cd/docker-compose.jev.yml`, `deploy/cd/build-cutover-bundle.sh`, `deploy/cd/deploy-lib.sh`. |
| O10 Integration tests fail without their database and avoid shared-fixture leakage | Verification support | #90, #238, #239, #265, #278, #350 | CHE-1669, CHE-1670, CHE-1676, CHE-1810 | fork-only | Keep database admission and fixture isolation. #265 is a one-off CI re-trigger, not a new runtime feature. Source: `server/cmd/server/integration_test.go`, `server/internal/handler/handler_test.go`, `server/internal/commentguard/lock_test.go`. |
| O11 Real-browser disclosure regressions and repaired upstream-sync E2E selectors/timeouts | Verification support | #14, #50, #63, #359 | CHE-268, CHE-463, CHE-554, CHE-1968 | fork-only | Retain behavioral assertions. Fixture repairs are not evidence that a fresh sync passed. Source: `e2e/issue-content-disclosure.spec.ts`, `e2e/quick-actions.spec.ts`, `e2e/local-search.spec.ts`, `packages/views/search/search-command.test.tsx`. |
| O12 Go/network dependency security update | Toolchain maintenance | #183 | CHE-1426 | overlaps ce5a2ba47 / upstream #9420 | Adopt the newer upstream Go build toolchain while preserving fork network security updates and CI routing. Source: `server/go.mod`, `.github/workflows/ci.yml`, `.github/workflows/release.yml`. |
| O13 Isolated paired-replay instrumentation | Token measurement | #348 | CHE-1942 | fork-only | Keep measurement isolation. Source: `scripts/paired-replay.mjs`, `scripts/paired-replay.md`. |
| O14 Fork build stamps and distinct server version/build/commit fields | Release identity | #179, #331, #368 | CHE-1310, CHE-1910 | fork-only | Keep upstream-based version plus ICT build stamp and commit identity. Source: `packages/views/layout/help-launcher.tsx`, `packages/views/runtimes/components/update-section.tsx`, `server/cmd/multica/cmd_version.go`, `server/internal/handler/config.go`, `scripts/release-stamp.sh`, `scripts/install-cli-from-ref.sh`. |
| O15 Fork changelog line contract, generation, preview and qualified release display | Release presentation | #228, #229, #231 | CHE-1632, CHE-1634, CHE-1637 | fork-only | Keep the fork release record; upstream release notes do not replace qualified fork entries. Source: `.github/PULL_REQUEST_TEMPLATE.md`, `.github/workflows/pr-changelog-check.yml`, `scripts/fork-changelog.mjs`, `apps/web/features/landing/components/changelog-page-client.tsx`. |
| O16 Autopilot cooldown lookup scope and unset probe wiring | Scheduling | #332 | CHE-1911, CHE-1926 | fork-only | Keep scoped cooldown lookup. Source: `server/internal/service/provider_failure_guard.go`, `server/internal/handler/handler.go`. |
| O17 Fork CLI/skill reference and child completion briefing updates | Agent documentation | #59, #74, #76 | CHE-559, CHE-605, CHE-616 | fork-only | Retain relevant fork command semantics. Source: `server/internal/service/builtin_skills/multica-platform/references/issues.md`, `server/internal/daemon/execenv/runtime_config_sections.go`. |
| O18 Sandbox namespace/cgroup feasibility packet, not an implemented isolation feature | Design evidence | #16 | CHE-332 | fork-only | Keep as feasibility evidence only. Source: `.planning/che-332-daemon-sandbox/D1-FEASIBILITY.md`. Design/feasibility evidence only; not a runtime entry point. |
| O19 Destructive development-startup GC requires explicit opt-in | Local environment safety | #280 | CHE-1813 | fork-only | Keep the explicit opt-in. A normal development start is not cleanup authorization. Source: `scripts/dev-env.sh`. |

## Design-only records

| Feature | Area | Fork PRs | CHE issues | Upstream status | Recommendation |
| --- | --- | --- | --- | --- | --- |
| X01 Decision quote/DM information-card direction | Design artifacts | #316 | CHE-1870 | fork-only | Retain the selected-direction record. Do not infer production implementation from mockups. Source: `design/dm-decision-quote-cards/rev-8/NOTES.md`, `design/dm-decision-quote-cards/rev-8/B.html`. Design/feasibility evidence only; not a runtime entry point. |
| X02 DM statistics/archive-menu exploration and admit sheets | Design artifacts | #259, #284, #328 | CHE-1756, CHE-1904, CHE-1908 | fork-only | Retain as approved/explored design context, not a claim that all menu/statistics surfaces shipped. Source: `docs/designs/dm-archive-statistics-backstack.md`, `design/dm-stats-archive-menu/README.md`. Design/feasibility evidence only; not a runtime entry point. |
| X03 DM search/notification/Web Push design and amendments | Design artifacts | #270, #271, #277 | CHE-1785, CHE-1790 | fork-only | Keep design context separate from the implemented search/notification rows. Web Push is not established by those rows. Source: `docs/designs/dm-search-notifications-push.md`, `design/dm-notifications-mobile/README.md`. Design/feasibility evidence only; not a runtime entry point. |
| X04 Bare issue-key chip stability directions | Design artifacts | #294 | CHE-1829 | fork-only | Retain design evidence only. Source: `design/bare-key-chip-stability/README.md`, `design/bare-key-chip-stability/rev-1/NOTES.md`. Design/feasibility evidence only; not a runtime entry point. |
| X05 DM follow-up/wakeup-chip directions | Design artifacts | #323 | CHE-1874 | fork-only | Retain design evidence only. Source: `design/dm-wakeup-chip/README.md`, `design/dm-wakeup-chip/rev-2/NOTES.md`. Design/feasibility evidence only; not a runtime entry point. |

## Candidate dispositions and unresolved product choices

The parent candidates are not all replacement equivalents:

- `482ec69c1` records claim outcomes, not the fork's claimed model.
- `e720fe221` adds explicit conflict watches, not CI-green workflow advance.
- `0beaeddc0` changes ownership permissions, not the fork CLI or per-issue override.
- `53c0088f1` adds terminal notices, not provider backoff or retry suppression.
- `0fc679482` upgrades an inherited equality filter. There is no independent
  fork comparison feature to delete. The upstream parser is the recommendation.

Upstream `51975f95f` supplies named-wakeup squad-leader attribution that the
fork's automatic built-in path does not supply for named wakeups. Combining
the paths must retain fork head/config/admission metadata (R06).
Upstream `ebc7c4696` fixes HTTP-code regex boundaries. That does not supersede
the provider-recovery family (R05).

Only two proposed simplifications need a product-direction choice:

1. R08: keep automatic owner PR alerts plus explicit watches (recommended),
   or replace automatic defaults with upstream opt-in conflict watches.
2. R09: use upstream wakeup activity/inbox without the legacy parent system
   comments (recommended), or retain those visible comments.

Existing notifications remain the baseline pending that choice. No row grants
permission to delete receipts, remove trust-boundary checks, merge a PR or
deploy production. The four upstream security fixes listed in CHE-2004 are
additive sync requirements, not fork features to retire.
