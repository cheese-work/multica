# Upstream-readiness audit

Companion to [FORK-FEATURES.md](FORK-FEATURES.md) (CHE-2071, refreshed by CHE-4789 at fork `e8f066f3ea`). It rates each ledger row and lists what is missing before an upstream PR.

## Method and limits

Static. No runtime qualification and no coverage run. Evidence per row is mechanical, taken from the fork PRs the ledger names:

- **Files** flags show which kinds of file the row's PRs touched: `T` tests, `U` web/desktop UI, `C` CLI, `D` docs, `M` migration, `I` CI/deploy, `S` other server code. `T` means a test file was touched, not that the behavior is covered.
- **complete**: fork-only or fully reconciled, its PRs touched tests, and no parity gap is known.
- **partial**: overlaps an upstream change that must be reconciled first, or its PRs touched no tests, or a CLI/UI/docs parity gap is known.
- **experimental**: design-only record or feasibility packet. **removed**: kept out on purpose.
- **Upstream fit**: `yes` stands alone; `needs design talk` is a subsystem upstream has not asked for; `fork integration` depends on Jev, Hermes or fork-only services; `fork ops only` is CI, deploy or release plumbing for this fork.
- Known-gap notes come from reading the CLI surface and builtin skill, not from reading every handler. A row with no note is not proof of parity.

- **Refresh (CHE-4789, 2026-11-09 08:44 ICT):** 248 rows: 168 carried over and 80 new. Ratings of carried-over rows are kept. Two Gaps cells are reworded for the new row C72 (C71, C54). Overlap re-checks (UPSTREAM-OVERLAP.md) changed no rating. New rows follow the rules below.
- **Earlier refresh (CHE-2792, 2026-11-02 16:33 ICT):** 168 rows, ratings of the 104 carried-over rows kept except where a removal or an upstream re-check (UPSTREAM-OVERLAP.md) changed them.
  New rows get `Files` from the fork PRs that name them, `Rating` by the rules above, and `Upstream fit` by the same definitions.
  A hard dependency on a removed row, and a partly removed row, are rated `partial`.
- **Dependencies** come from FORK-DEPENDENCIES.md. A hard dependency orders the pitch list below; a soft one only adds a note.

## Totals

| Rating | Rows |
| --- | --- |
| complete | 167 |
| experimental | 18 |
| partial | 54 |
| removed | 9 |

| Upstream fit | Rows |
| --- | --- |
| fork integration | 14 |
| fork ops only | 38 |
| n/a | 29 |
| needs design talk | 65 |
| yes | 102 |

Board lanes (rules in the next section; counts in the board attached to CHE-4789):

| Lane | Rows | Rule |
| --- | --- | --- |
| Ready to pitch | 49 | fit yes, rating complete, fork-only, present on main, and every hard dependency is itself in this lane or already upstream |
| Reconcile first | 28 | fit yes but upstream covers part of it; merge choice first |
| Close gaps first | 25 | fit yes but rating partial, partly removed, or a hard dependency is not ready |
| Needs design talk | 65 | subsystem upstream has not asked for; start a design talk |
| Stays in fork | 52 | fork integration or fork ops; stays in the fork (also every fork-only fix) |
| Dead pool | 29 | removed, upstreamed, design-only or record-only rows |

Fix lane (not rows): see [UPSTREAM-FIXES.md](UPSTREAM-FIXES.md).

## Pitch order for `fit: yes` rows

Order: dependency depth first (a row comes after every row it hard-depends on), then lane (ready, close gaps, reconcile), then ID.
Rows in a hard-dependency cycle share a depth and must land together or after a hook is split out.
`Needs first` lists hard dependencies; a dependency that is not `fit: yes` is named with its fit, and blocks the row until it is resolved.

| # | Row | Lane | Rating | Needs first | Cycle with |
| --- | --- | --- | --- | --- | --- |
| 1 | C04 | Ready to pitch | complete | - | - |
| 2 | C06 | Ready to pitch | complete | - | - |
| 3 | C12 | Ready to pitch | complete | - | - |
| 4 | C13 | Ready to pitch | complete | - | - |
| 5 | C16 | Ready to pitch | complete | - | - |
| 6 | C17 | Ready to pitch | complete | - | - |
| 7 | C19 | Ready to pitch | complete | - | - |
| 8 | C20 | Ready to pitch | complete | - | - |
| 9 | C21 | Ready to pitch | complete | - | - |
| 10 | C24 | Ready to pitch | complete | - | - |
| 11 | C33 | Ready to pitch | complete | - | - |
| 12 | C37 | Ready to pitch | complete | - | - |
| 13 | C38 | Ready to pitch | complete | - | - |
| 14 | C39 | Ready to pitch | complete | - | - |
| 15 | C40 | Ready to pitch | complete | - | - |
| 16 | C42 | Ready to pitch | complete | - | - |
| 17 | C44 | Ready to pitch | complete | - | - |
| 18 | C48 | Ready to pitch | complete | - | - |
| 19 | C56 | Ready to pitch | complete | - | - |
| 20 | C63 | Ready to pitch | complete | - | - |
| 21 | C64 | Ready to pitch | complete | - | - |
| 22 | C66 | Ready to pitch | complete | - | - |
| 23 | C67 | Ready to pitch | complete | - | - |
| 24 | C68 | Ready to pitch | complete | - | - |
| 25 | C69 | Ready to pitch | complete | - | - |
| 26 | C77 | Ready to pitch | complete | - | - |
| 27 | D03 | Ready to pitch | complete | - | - |
| 28 | D04 | Ready to pitch | complete | - | - |
| 29 | D05 | Ready to pitch | complete | - | - |
| 30 | D07 | Ready to pitch | complete | - | - |
| 31 | D08 | Ready to pitch | complete | - | - |
| 32 | D10 | Ready to pitch | complete | - | - |
| 33 | D13 | Ready to pitch | complete | - | - |
| 34 | D21 | Ready to pitch | complete | - | - |
| 35 | G03 | Ready to pitch | complete | - | - |
| 36 | G28 | Ready to pitch | complete | - | - |
| 37 | G29 | Ready to pitch | complete | - | - |
| 38 | W11 | Ready to pitch | complete | - | - |
| 39 | W14 | Ready to pitch | complete | - | - |
| 40 | W18 | Ready to pitch | complete | - | - |
| 41 | W22 | Ready to pitch | complete | - | - |
| 42 | W27 | Ready to pitch | complete | - | - |
| 43 | C05 | Close gaps first | complete | - | B01, B02 |
| 44 | C15 | Close gaps first | partial | - | - |
| 45 | C22 | Close gaps first | partial | - | - |
| 46 | D25 | Close gaps first | partial | - | - |
| 47 | H09 | Close gaps first | complete | - | H10 |
| 48 | W06 | Close gaps first | partial | - | W08, W09 |
| 49 | W08 | Close gaps first | partial | - | W06, W09 |
| 50 | W09 | Close gaps first | partial | - | W06, W08 |
| 51 | W16 | Close gaps first | complete | - | W15 |
| 52 | W20 | Close gaps first | complete | - | H07 (n/a, removed writer; helper survives) |
| 53 | C01 | Reconcile first | partial | - | - |
| 54 | C02 | Reconcile first | partial | - | - |
| 55 | C03 | Reconcile first | partial | - | - |
| 56 | C14 | Reconcile first | partial | - | - |
| 57 | C18 | Reconcile first | partial | - | - |
| 58 | C43 | Reconcile first | partial | - | - |
| 59 | C46 | Reconcile first | partial | - | - |
| 60 | C47 | Reconcile first | partial | - | - |
| 61 | C73 | Reconcile first | partial | - | - |
| 62 | D01 | Reconcile first | partial | - | - |
| 63 | D06 | Reconcile first | partial | - | - |
| 64 | D16 | Reconcile first | partial | - | - |
| 65 | H01 | Reconcile first | partial | - | - |
| 66 | H03 | Reconcile first | partial | - | - |
| 67 | H04 | Reconcile first | partial | - | - |
| 68 | H05 | Reconcile first | partial | - | - |
| 69 | W01 | Reconcile first | partial | - | - |
| 70 | W02 | Reconcile first | partial | - | - |
| 71 | W03 | Reconcile first | partial | - | - |
| 72 | W12 | Reconcile first | partial | - | - |
| 73 | W13 | Reconcile first | partial | - | - |
| 74 | C11 | Ready to pitch | complete | - | C34 |
| 75 | C27 | Ready to pitch | complete | C24, C06 | - |
| 76 | C31 | Ready to pitch | complete | C12, C19 | - |
| 77 | C34 | Ready to pitch | complete | C12 | C11 |
| 78 | C45 | Ready to pitch | complete | C44, C67 | - |
| 79 | D15 | Ready to pitch | complete | D21 | - |
| 80 | H11 | Ready to pitch | complete | D08 | - |
| 81 | D14 | Close gaps first | complete | D06 | - |
| 82 | H12 | Close gaps first | complete | H05 | - |
| 83 | W04 | Close gaps first | partial | W15 (needs design talk) | - |
| 84 | W05 | Close gaps first | complete | W06, W08, W09, W11 | - |
| 85 | W07 | Close gaps first | partial | W06 | - |
| 86 | W19 | Close gaps first | complete | W15 (needs design talk) | - |
| 87 | W26 | Close gaps first | complete | W15 (needs design talk) | - |
| 88 | C35 | Reconcile first | partial | C12 | - |
| 89 | C36 | Reconcile first | partial | C12, C15 | - |
| 90 | H02 | Reconcile first | partial | C03 | - |
| 91 | H08 | Reconcile first | partial | C05 | - |
| 92 | W10 | Reconcile first | partial | W06 | - |
| 93 | W31 | Reconcile first | partial | W02 | - |
| 94 | W21 | Reconcile first | partial | W06, W07, W08, W09 | - |
| 95 | C08 | Close gaps first | complete | C06 | B03, C10, C25, C26, C28, C29 |
| 96 | C10 | Close gaps first | complete | C06 | B03, C08, C25, C26, C28, C29 |
| 97 | C25 | Close gaps first | complete | C06, C24 | B03, C08, C10, C26, C28, C29 |
| 98 | D09 | Close gaps first | complete | G04 (needs design talk) | - |
| 99 | C07 | Close gaps first | complete | C06, C08, C25 | - |
| 100 | C09 | Close gaps first | complete | C10, C29 (needs design talk), C05, C24, C28 (needs design talk) | - |
| 101 | C32 | Close gaps first | complete | C25, C20, C11 | - |
| 102 | C41 | Close gaps first | complete | C25 | - |

## Rows

| Row | Feature | Rating | Upstream fit | Files | Gaps |
| --- | --- | --- | --- | --- | --- |
| C01 | Description and long-thread disclosure, chronological reply/run slots and focus-safe fold controls | partial | yes | TUDI | reconcile with upstream first (f896cc483 / MUL-8074; f1560daff) |
| C02 | Stale resolve rejection and serialized concurrent replies | partial | yes | TCS | reconcile with upstream first (4b8873df3) |
| C03 | Stage-completion dedupe, generation receipts and parent system comments | partial | yes | TUDMS | reconcile with upstream first (5ed09342a / MUL-7680) |
| C04 | Deterministic status, ETA, active-run, CI and PR-head answers without an agent turn | complete | yes | TCDS | - |
| C05 | Verified DM authentication provenance and durable channel origin | complete | yes | TUDMIS | - |
| C06 | Task-bound decision intents, schema constraints and asked-record guards | complete | yes | TUCDMS | - |
| C07 | Decision records, human disclaim, veto and raise-only decision classes | complete | yes | TUCDS | - |
| C08 | Transactional decision delivery, inbox receipts, held groups and leased consequences | complete | yes | TUCDMS | - |
| C09 | Single-call decision requests, intent reuse and contextual cancellation | complete | yes | TUCDS | - |
| C10 | Decision ping unread state and DM-list refresh | complete | yes | TUDS | - |
| C11 | DM continuation, saved compact prompts, slash command, announcements and pin preservation | complete | yes | TUCDMS | - |
| C12 | Automatic compaction settings, prompt-size reporting, context meter and manual compact action | complete | yes | TUCDMS | - |
| C13 | Full-copy DM forks, optional first message, attachment transfer and fork-point links | complete | yes | TUCDMS | - |
| C14 | DM rename without an agent turn | partial | yes | TUMS | reconcile with upstream first (51aa92412); `multica chat rename` exists and is documented; the gap is the R20 merge with the upstream title API. |
| C15 | DM search endpoint and anchored message navigation | partial | yes | TUMS | No `chat search` verb. |
| C16 | Per-channel notification settings and coalesced DM inbox replies, off by default | complete | yes | TUDMS | - |
| C17 | Web and desktop DM back-stack restoration | complete | yes | TUS | - |
| C18 | Archived DMs are unpinned and cannot be re-pinned | partial | yes | TMS | reconcile with upstream first (a51ab4d55 / MUL-4171); No CLI archive/pin; `chat.sql` has no sibling test file; needs R20 merge. |
| C19 | DM message bubble timestamps in the client locale | complete | yes | TU | - |
| C20 | DM annotations analogous to issue reply annotations | complete | yes | TUDS | - |
| C21 | Failed issue-mention hydration recovers on focus | complete | yes | TUD | - |
| C22 | Workspace site-search URLs, Cmd+K and OpenSearch discovery | partial | yes | TUD | `opensearch.xml` path in ledger not found at HEAD; verify before citing. |
| C23 | Retired lane summaries and turn reminders | removed | n/a | TUCDMS | - |
| C24 | Structured decision ask: schema decoder and lint, withdraw verb, source chip, stale-ask digest, human-mention recommendation | complete | yes | TUCDMS | - |
| C25 | Decision quote cards, answer-on-card without a DM-agent run, answer receipts and picked-up-by-run display | complete | yes | TUDMS | - |
| C26 | Needs-you and silence lifecycle badges and pending-decision banner on the issue page | complete | needs design talk | TUS | - |
| C27 | Approval artifacts: artifacts[] with hash checks, You-are-approving card, fullscreen viewer, jump links and image paste | complete | yes | TUCDS | - |
| C28 | Decision routing: one topic DM per root issue with steward, open-asks read, evening brief, answer change feed, project delegates, Markdown record | complete | needs design talk | TUCDMIS | - |
| C29 | Approve-to-execute: executable decision actions, action registry, issue targets, park issue while a decision is open | complete | needs design talk | TCMS | - |
| C30 | Human decision authority: isolation, agent-instance freeze, commit-reveal signatures, passkey gate, per-user session revoke, status-change history | partial | needs design talk | TUDMS | hard dependency on removed row G13 |
| C31 | Agent-scheduled DM wakeup, Scheduled chip, fired wakeup as a system event row | complete | yes | TUCDMS | - |
| C32 | Predecessor DMs shown inline, collapsed and quotable | complete | yes | TU | - |
| C33 | DM reading position restored on remount and jump-to-latest button | complete | yes | TUD | - |
| C34 | In-place compact: compaction epoch, epoch-bound resume, claim/compact serialisation | complete | yes | TCDMS | - |
| C35 | Supplement steer for running DM tasks and follow-up delivery state | partial | yes | TUMS | reconcile with upstream first (packages/views/chat/components/chat-queue.tsx) |
| C36 | DM statistics dialog, archive and pin commands, archived send on web and desktop | partial | yes | TUD | reconcile with upstream first (apps/mobile/app/(app)/[workspace]/(tabs)/chat.tsx) |
| C37 | Authenticated address-bar suggestions | complete | yes | TUDIS | - |
| C38 | Highlight own mentions and comments (web, mobile, @all) and jump to next mention | complete | yes | TUD | - |
| C39 | Comment-ID chips with issue key, author and preview, and rich wakeup prompts with issue chips | complete | yes | TUS | - |
| C40 | Thread context on mention, resolve step and issue status summary | complete | yes | TUCDS | - |
| C41 | Plan revision compare and a new revision resolves the superseded revision's threads | complete | yes | TUCDMS | - |
| C42 | Permission-checked media viewer pages | complete | yes | TUD | - |
| C43 | Agent comment wakes are gated and agent comments that ask a human need mention or context | partial | yes | TUCMS | reconcile with upstream first (MUL-4525) |
| C44 | Lazy Relations graph, CLI and fast native Relations report | complete | yes | TUCDMS | - |
| C45 | Hierarchy minimap in the issue sidebar and canvas dialog | complete | yes | TUD | - |
| C46 | Mermaid diagrams: container-width orientation setting, gesture zoom engine, on-brand theme | partial | yes | TUD | reconcile with upstream first (packages/views/editor/hooks/use-zoom-canvas.ts) |
| C47 | Block agent activity in inactive projects and HTTPS URL project resources | partial | yes | TUCDMS | reconcile with upstream first (server/internal/handler/project_resource.go) |
| C48 | Absolute web_url in chat, issue, comment and attachment CLI output | complete | yes | TCS | - |
| C49 | Web Push subscriptions, outbox and VAPID worker | complete | needs design talk | TUDMIS | - |
| C50 | Decision action proposals: agent proposal route with scoped replay identity, proposal guard (cooldown, untrusted note), inline in the DM, proposal cards with passkey approval | complete | needs design talk | TUMS | - |
| C51 | Config-change action core: digests, server preview, denylist, audit, squad and workspace targets with owner-only flag, request-card preview, raw text and list diff, field binding and proposer Authorize | complete | needs design talk | TUS | - |
| C52 | Config set actions: workflow.steps.set, workspace.settings.patch, agent.instructions.set, squad.instructions.set and workspace.context.set | complete | needs design talk | TS | - |
| C53 | Human-in-charge resolver, endpoint and CLI (decision-wait L5a) | complete | needs design talk | TDCS | - |
| C54 | decision_wait episodes (L5b to L5e): table, entry check, delivery, suspension, system close, reminder and expiry exemption, wake receipts with retry | complete | needs design talk | TUMS | C08 reads episodes back (soft edge from C08 to C54) |
| C55 | Agent decisions: owner switch, test_first_red gate, Jev delegation, brief waiver and Jev metrics | complete | needs design talk | TUDCS | core PR #665 not in any row |
| C56 | decision_ask MCP tool and one-step CLI front for decision requests | complete | yes | TDCS | - |
| C57 | System 1 shadow capture of routing and decision data, CLI shadow-report | experimental | needs design talk | TDCMS | shadow capture only |
| C58 | Passkey approval component and passkey-gated decision card answers | complete | needs design talk | TUMS | - |
| C59 | Passkey settings page | complete | needs design talk | TUS | - |
| C60 | C00 passkey recovery command and session generation handling | complete | fork ops only | TMS | - |
| C61 | DM attention backend: storage and projection, idempotent send, Xong, Để sau and Xong tất cả endpoints, session-list fields, turn outcomes and task-end recovery | complete | needs design talk | TUMS | - |
| C62 | DM attention CLI and web Needs you box: reply-state, chat attention and later, attention rows with unclassified group | complete | needs design talk | TUDCS | - |
| C63 | Drafts: keyed draft store, mobile disk persistence, keep draft on invalid_agent_mentions | complete | yes | TU | - |
| C64 | Recent agents ranking and New chat Recent section | complete | yes | TU | - |
| C65 | Status history completion: handoff-comment link, failed-task resets, workflow remap, PR auto-complete in the status transaction, dialog with scrubber, listener-fallback removal | complete | needs design talk | TUS | core PR #773 sits in C30 title only |
| C66 | Actual-timeline server core with scope, filters and keyset paging | complete | yes | TS | - |
| C67 | Timeline client: graph core, canvas engine split with unbounded zoom, Swimlane 2.0 layout | complete | yes | TU | client not wired to C66 in code read |
| C68 | Search: typo-tolerant fallback and Vietnamese diacritic folding | complete | yes | TMS | - |
| C69 | DM mark-unseen: server endpoint, CLI, web button and /unseen, PAT-caller own-DM list with unread | complete | yes | TUDCS | - |
| C70 | Chat task purpose, segment floor and Rule W, purpose-first authority for chat reads, compact and continue | complete | needs design talk | TMS | - |
| C71 | In-transaction DM progress event capture | partial | fork integration | TMS | no non-test reader of dm_progress_pending found; capture reads dm_progress_follow (C72) |
| C72 | DM progress follow | complete | needs design talk | TDCMS | - |
| C73 | Server rejects agent comments that mention non-invocable agents; CLI reports inv | partial | yes | TCS | reconcile with upstream preview first |
| C74 | Decision wake events and per-recipient wake receipts | complete | needs design talk | TMS | - |
| C75 | Ask guard | complete | needs design talk | TDCMS | - |
| C76 | System 1 shadow routing | complete | fork integration | TDCMS | - |
| C77 | Issue effective_due computed on read with cycle and depth diagnostics; issue lis | complete | yes | TUDCS | - |
| H01 | Multi-organization GitHub installations with verified account authority | partial | yes | TUS | reconcile with upstream first (caeb146ba) |
| H02 | Explicit PR close intent, quoted-code stripping and open-child completion gate | partial | yes | TDS | reconcile with upstream first (0da0b46bb / MUL-7672) |
| H03 | Merged PRs advance custom done-category review states | partial | yes | TUS | reconcile with upstream first (fc4a61570 / MUL-7726) |
| H04 | Actor/task-aware automatic PR linking and CLI unlink repair | partial | yes | TUCS | reconcile with upstream first (0da0b46bb / MUL-7672) |
| H05 | Snapshot refresh for already-decided PRs and latest-run-per-name/app check filtering | partial | yes | TUMS | reconcile with upstream first (e720fe221 / MUL-8135; ecce58986 / MUL-5265) |
| H06 | PR close-target validation and non-leaking sparse checkout | complete | fork ops only | TUI | tests in its PRs: `scripts/pr-close-target-check.test.mjs` (the earlier file-name check missed them). |
| H07 | Durable merge-announcement writer, subsequently removed | removed | n/a | TUCDMIS | removed from main: 149e45f53e refactor(github): remove fork merge-announcement comment (#154) |
| H08 | CLI browser handoff exchanged for a CLI token | partial | yes | TUDS | reconcile with upstream first (da278c7d4 / MUL-8154) |
| H09 | Wake PR author agents on human reviews | complete | yes | TUS | - |
| H10 | GitHub intake: signed delivery store, fork-PR denial, reconciler, route grants, quarantine and snapshot approval, kill switch, intake list | complete | needs design talk | TUCDMS | - |
| H11 | PR events captured, queued for the next turn and delivered to live runs; reviews bound to a PR head; merged/closed PR stops work | complete | yes | TUDMS | - |
| H12 | GitHub account allowlist | complete | yes | TMS | - |
| H13 | GitHub dispatch halt | complete | fork integration | TCMS | - |
| H14 | GitHub App installation allowlist | complete | needs design talk | TUDS | - |
| W01 | CLI read/edit of project workflows | partial | yes | TCD | reconcile with upstream first (0beaeddc0 / MUL-8120; da1671ca2 / MUL-7420) |
| W02 | Per-issue workflow override and guarded reassignment | partial | yes | TUCMS | reconcile with upstream first (da1671ca2 / MUL-7420) |
| W03 | `--no-start` preserves a changed handler's workflow handoff | partial | yes | TS | reconcile with upstream first (da1671ca2 / MUL-7420) |
| W04 | Green-check owner wakeup and optional workflow auto-advance | partial | yes | TUMS | `issue_workflow_ci_advance.go` has no sibling test file; needs R02 decision. |
| W05 | Automatic PR merge/failure/attention wakeups, including base-branch and accepted-to-draft changes | complete | yes | TUDMIS | - |
| W06 | Built-in definition model, scoped cascade, CRUD/preview and legacy projection/backfill | partial | yes | TUMS | No CLI for wakeup definitions (project/workspace wakeups still missing). |
| W07 | Separate local/default wakeup capacity pools | partial | yes | TUMS | No CLI to see pool usage. |
| W08 | Aggregate wakeup admission budgets | partial | yes | TUMS | No CLI to set aggregate budgets. |
| W09 | Explicit suppress/defer when an agent already has active work | partial | yes | TUMS | No CLI to see the suppress/defer disposition. |
| W10 | Atomic recovery from PR-wakeup rate pause | partial | yes | TUMS | reconcile with upstream first (5ed09342a / MUL-7680) |
| W11 | Workspace-wide wakeup firing pause | complete | yes | TUCDIS | - |
| W12 | Wakeup activity identifies trigger, agent and run | partial | yes | TUDS | reconcile with upstream first (5ed09342a / MUL-7680) |
| W13 | Comment triggers fold into queued work, wake blocked leaders and avoid author self-trigger | partial | yes | TUCDIS | reconcile with upstream first (16f0d307dd (#9702); MUL-4525) |
| W14 | Idle squad-leader failure markers | complete | yes | TUCMIS | - |
| W15 | Per-step guarded edges, edge engine (entry, timeouts, child PRs), GitHub review guards and read-only flowchart view | complete | needs design talk | TUCDMS | - |
| W16 | Sub-workflow node with child spawn and stage join | complete | yes | TUS | - |
| W17 | Inbound workflow webhook: signature and payload verifier, endpoint, nonce and event tables | complete | needs design talk | TCDMS | - |
| W18 | Human-by-role handler and requester trace | complete | yes | TUCMS | - |
| W19 | Step entry gates (engine and API) and design | complete | yes | TUCDS | - |
| W20 | GitHub Actions run wakeup condition, `--until-gh-run` flag and workflow run/job tables | complete | yes | TUCDMS | - |
| W21 | Scoped-event outbox, custom event/condition rules and scheduled wakeups (at, relative, every, cron) | partial | yes | TUMS | reconcile with upstream first (5ed09342a / MUL-7680) |
| W22 | Machine pause claim gate, daemon pause reconciliation and scheduling-pause data model | complete | yes | TUDMS | - |
| W23 | Scoped wakeup rules UI | complete | needs design talk | TUDCMS | no live UI run; static read only |
| W24 | Agent-created workspace and project wakeup rules are stored disabled until a hum | complete | needs design talk | TDCMS | - |
| W25 | Workflow edge guards | complete | needs design talk | TUCMS | - |
| W26 | Workflow lint engine with structural rules and instruction rules WF009/WF010, li | complete | yes | TDCS | - |
| W27 | Per-workflow start status so autopilot briefs skip the todo pull | complete | yes | TCMS | - |
| W28 | Skip a no-change wakeup dispatch before the LLM (input fingerprint, 6 h window,  | complete | needs design talk | TS | - |
| W29 | Evidence waiver creates a follow-up check wakeup, one per waiver | complete | fork integration | TDMS | - |
| W30 | Stage queue WIP limits for review, device and browser gates | complete | needs design talk | TMS | - |
| W31 | Workflow choice surfaces: mobile override, batch toolbar, create-issue dialog, list and board filter and pill | partial | yes | TUS | reconcile with upstream project workflows (da1671ca2 / MUL-7420) |
| W32 | Workflow lint agent and runtime rules | complete | fork integration | TS | - |
| W33 | CI failure watcher groundwork: tables, flag, budget ledger, stacks client | partial | fork integration | TMS | no non-test caller of ghbudget or the stacks client found; flag off |
| G01 | Field-scoped agent denial for governed instruction writes and the Hermes exception | partial | needs design talk | TDS | No sibling test file for `governed_instruction_fields.go`. |
| G02 | Owner-only digest compare-and-swap for workspace/squad/agent instructions | complete | needs design talk | TUCS | - |
| G03 | Lossless file/stdin squad instruction input | complete | yes | TUCD | - |
| G04 | Two-agent governed-write votes and CLI vote receipts | complete | needs design talk | TUCMS | - |
| G05 | Required source-link enforcement on the server | complete | needs design talk | TCS | - |
| G06 | Deterministic protocol lint, persisted violations, omission reporting and complete waiver lookup | complete | needs design talk | TUDMS | - |
| G07 | Checkpoint-based briefing, reduction and claim-time persistence | complete | needs design talk | TUDMS | - |
| G08 | Typed duplicate admission for issue assignment, rerun lineage and autopilot-created issues | partial | needs design talk | TUCDMS | reconcile with upstream first (MUL-4525) |
| G09 | Provider hold across dispatch, manual rerun, retry and claim | complete | needs design talk | TUDS | - |
| G10 | Jev flags, scoped client, state budget, production/no-key gate and missing-mention evaluation | removed | n/a | TUDIS | removed from main: 1cb8be9138 (jev.go, featureflags/jev.go), 3e244342c9 (governance/evaluator.go, govreplay) both CHE-2760 |
| G11 | Jev routing receipts independent of published comments | removed | n/a | TMS | removed from main: 1cb8be9138 (comment_governance_receipt.go), 3e244342c9 (governance/receipt/receipt.go) both CHE-2760 |
| G12 | Canonical inherited rule revision resolution | removed | n/a | TUS | removed from main: 3e244342c9 delete dead governance evaluator/receipt/credential packages (CHE-2760) |
| G13 | Workspace governance control gate and proposal task credential restriction | removed | n/a | TUDMS | removed from main: 1cb8be9138 delete dead Jev/governance handlers, routes, wiring (CHE-2760) |
| G14 | Immutable cases, fenced lifecycle transitions and expired-evidence refresh | removed | n/a | TUMS | hard dependency on removed row G13; removed from main: 45778e7d9f delete dead governance caselifecycle and pkg/jev (CHE-2760) |
| G15 | Case evidence redaction, retention and audited provenance export | partial | needs design talk | TUCDMS | partly removed from main: 1cb8be9138 deleted handler/governance_case_audit.go (case-audit handler) |
| G16 | Export privacy modes, retention and admin controls | complete | needs design talk | TUMS | - |
| G17 | Atomic case admission and budget reservation across windows, crash recovery and bounded ledger queries | removed | n/a | TUCDMIS | hard dependency on removed row G14, G13; removed from main: 3e244342c9 deleted governance/admission.go, budget.go, budget_accounting.go |
| G18 | Bounded observation, rollback deadlines, timeout classification and real-budget test controls | removed | n/a | TUCDMS | hard dependency on removed row G10; removed from main: 1cb8be9138 deleted handler/comment_governance_receipt.go |
| G19 | Jev squad-leader routing, shadow mode, error handling and trigger shadow ledger | partial | fork integration | TUCDMS | hard dependency on removed row G13, G10; partly removed from main: 1cb8be9138 deleted handler/squad_jev_routing.go and squad_jev_shadow.go |
| G20 | Jev mode/confidence, credential, rollback and API-URL controls | partial | fork integration | TUDS | hard dependency on removed row G13; partly removed from main: 6384621458 MJ removal C2b remove Settings Jev / Governance tab (CHE-2761) |
| G21 | Owner maintenance and conditional agent environment controls | complete | fork integration | TUCDS | - |
| G22 | Credential-exclusive quota-runner execution | complete | fork integration | TUCDS | - |
| G23 | Leader reconciliation proof and durable event-key precedence in briefing | complete | needs design talk | TUDS | - |
| G24 | Decision-bound agent writes on governed instruction fields and workflow routes, delegated-write setting, `--decision-id` dry run | complete | needs design talk | TCDMS | - |
| G25 | Jev and governance removal (MJ removal): dead handlers, Settings tab, squad/comment dispatch unhooked, token estimate relocated | complete | n/a | TUDS | - |
| G26 | Decision-action executors | complete | needs design talk | TS | - |
| G27 | Governed PASS-receipt writes for `agent.instructions` and `workflow.instructions | complete | fork integration | TMS | - |
| G28 | Workspace audit event log | complete | yes | TMS | - |
| G29 | Task-token write-scope deny/audit for write routes, with indirect-write checks | complete | yes | TS | - |
| G30 | Config-sync L1 | complete | fork integration | TMIS | - |
| G31 | Configurable routing agent setting with fallback and audit | complete | fork integration | TUDCS | - |
| D01 | Atomic daemon single-instance startup and PID publication safety | partial | yes | TUCS | reconcile with upstream first (191c782d6 / MUL-7988) |
| D02 | Fork daemon artifact distribution, server-hosted builds and install-while-busy | partial | fork ops only | TUCDIS | reconcile with upstream first (fcd13aece / MUL-2100) |
| D03 | Per-agent Hermes external-memory opt-in | complete | yes | TUCDS | - |
| D04 | Codex SQLite-init retry, false-negative tool-access handling, forced model catalog and poisoned multi-agent resume avoidance | complete | yes | TUDS | - |
| D05 | AGY managed MCP config and retirement of poisoned executor/malformed-call sessions | complete | yes | TUMS | - |
| D06 | Pre-LLM provider recovery, gateway backoff and retry-pending coordinator suppression | partial | yes | TUDMS | reconcile with upstream first (53c0088f1 / MUL-7847; ebc7c4696) |
| D07 | Oversized leader issue sessions start fresh | complete | yes | TS | - |
| D08 | Ignore repeat delivery of an already executing task | complete | yes | TS | - |
| D09 | Claimed-model readback alongside claim provenance | complete | yes | TUS | - |
| D10 | Agent-skill runtime target setting from the CLI | complete | yes | TC | - |
| D11 | Price model-specific Hermes/Codex usage | complete | n/a | TUS | already upstream (ba30324e10) |
| D12 | Bounded WeCom inbox acceptance wait | complete | fork ops only | TS | - |
| D13 | Skip Claude resume when the prior session is idle past the 1 h cache TTL; opt-in stable per-agent cwd | complete | yes | TS | - |
| D14 | One deferred retry for 429 capacity failures and reservation schema with representable effects | complete | yes | TMS | - |
| D15 | Two-tier silence watchdog, stalled-tool marker and silence-alert endpoint | complete | yes | TUDMS | - |
| D16 | Commit-based runtime update status and self-update refusal under `UPDATE_FREEZE` | partial | yes | TUCDIS | reconcile with upstream first (see UPSTREAM-OVERLAP.md) |
| D17 | Daemon host sample on heartbeat, ops snapshot sampler, requeue events and `/api/ops` | complete | needs design talk | TMS | - |
| D18 | Claim ledger and admission store (recovery layer 2) | complete | needs design talk | TMS | - |
| D19 | Idle started-issue guard with off, shadow or act mode, settable from Settings and the CLI | complete | needs design talk | TDS | - |
| D20 | Agent presets: data model, resolver, API, CLI, `/preset` command, web picker; applied launch settings per claim | complete | needs design talk | TUCDMS | - |
| D21 | Shared provider-token redaction rules between Go and TypeScript | complete | yes | TUIS | - |
| D22 | Download progress for runtime updates: CLI reporter, daemon throttled reports, server-stored progress, install scripts, desktop stream, Runtimes progress bar | complete | fork integration | TUCIS | - |
| D23 | Provider circuit breaker: per workspace and provider state, trip rule, claim gate, notice, state API, admin reset, audit, CLI and settings panel | complete | needs design talk | TUCDMS | - |
| D24 | Runtime profile setting: personal or managed harness config home | complete | needs design talk | TUDS | managed profile supports claude and codex only |
| D25 | initctx package: per-part token estimate and budget report for the agent init context | partial | yes | TS | no non-test caller found at main |
| D26 | Background agents | complete | needs design talk | TMS | - |
| B01 | Run budget core: locked admission ledger, token reservation and settlement, per-workflow token activation | complete | needs design talk | TMS | - |
| B02 | Run budget notices, run-prompt budget line, snapshot endpoint, raise math and CLI get | complete | needs design talk | TCDMS | - |
| B03 | Member top-up endpoint and CLI, blocked-recovery reserve, server-origin exhaustion decision request | complete | needs design talk | TCDMS | - |
| B04 | Passkey-approved run-budget refusal record, raise action and top-up action | complete | needs design talk | TMS | - |
| B05 | Request a run-budget raise or top-up: issue endpoint, CLI verbs, `multica decision propose`, decision action panel | complete | needs design talk | TUCDS | - |
| S01 | Sandbox launcher, run capability grants, profile resolver, env allowlist and standalone per-task `.git` | partial | needs design talk | TMS | reconcile with upstream first (see UPSTREAM-OVERLAP.md) |
| S02 | Sandbox network isolation: netns loopback forwarder, host egress proxy, elevated egress policy, connectivity exit gate | complete | needs design talk | TCDS | - |
| S03 | Elevated read path: read proxy, server revision check, scoped snapshot approval, elevation state, intake-run pinning, synthetic base repo | complete | needs design talk | TCMS | - |
| S04 | Taint tracking: task/comment taint fields, issue-level taint with member clear, external_read marks | complete | needs design talk | TMS | - |
| S05 | Push broker: socket, bundle receive, ref and repo pin, push-token lifecycle, protected-path checks, task-token write scoping | complete | needs design talk | TCDS | - |
| S06 | Broker daemon fence, commit translation on push, receipts on translated SHAs and base refresh | partial | needs design talk | TS | no non-test caller of broker.New found at main; daemon launcher not wired |
| S07 | Restricted read token mint via broker, gh shim refresh and revoke | partial | needs design talk | TCS | no non-test caller of broker.New found at main; daemon launcher not wired |
| S08 | Read proxy stubs for PR title and body, CI log and commit message | partial | needs design talk | TS | readproxy server not started by any non-test code found; only ShimMain is imported |
| S09 | Multica read filter: stub marked or tainted bodies, approve and downgrade path | complete | needs design talk | TS | - |
| S10 | Elevated runs start with provider web tools off; bypass matrix and X99 gate harness | partial | needs design talk | TS | gatesuite has no non-test caller found |
| O01 | X99 build/CI ownership, runner migration and shared-Postgres/turbo contention limits | complete | fork ops only | TUDMIS | - |
| O02 | Web build CPU/worker OOM limits | complete | fork ops only | TUD | - |
| O03 | Immutable image qualification and qualified C00 delivery admission | complete | fork ops only | TUDIS | - |
| O04 | D2 migration quiescence and retained-archive rollback safety | complete | fork ops only | TUDMIS | - |
| O05 | Blue/green application slots, router state, atomic cutover and correct rollback tuple | complete | fork ops only | TUDI | - |
| O06 | C00 queue stall alarms and superseded-candidate skipping | complete | fork ops only | TUDI | tests in its PRs: `deploy/cd/test-check-deploy-queue.py, deploy/cd/test-candidate-preflight.py` (the earlier file-name check missed them). |
| O07 | Bounded candidate image-pull retry | complete | fork ops only | TUDI | - |
| O08 | Historical migration identity reconciliation | complete | fork ops only | TUDMIS | - |
| O09 | Scoped Jev environment inputs survive backend recreation | partial | fork ops only | TDI | partly removed from main: 1cb8be9138 / PR #703 (CHE-2760) deleted server/cmd/server/jev.go, so no MULTICA_JEV_SECRET_KEY consumer remains in server/ |
| O10 | Integration tests fail without their database and avoid shared-fixture leakage | complete | fork ops only | TUDMIS | - |
| O11 | Real-browser disclosure regressions and repaired upstream-sync E2E selectors/timeouts | complete | fork ops only | TUD | - |
| O12 | Go/network dependency security update | complete | n/a | UDS | already upstream (ca593b3103; 3bacc41f84; ce5a2ba472) |
| O13 | Isolated paired-replay instrumentation | complete | fork ops only | TUD | tests in its PRs: `scripts/paired-replay.test.mjs` (the earlier file-name check missed them). |
| O14 | Fork build stamps and distinct server version/build/commit fields | complete | fork ops only | TUCDIS | - |
| O15 | Fork changelog line contract, generation, preview and qualified release display | complete | fork ops only | TUDIS | - |
| O16 | Autopilot cooldown lookup scope and unset probe wiring | complete | fork ops only | TUCS | - |
| O17 | Fork CLI/skill reference and child completion briefing updates | partial | fork ops only | TUDS | reconcile with upstream first (90a88519df; a008d893dd) |
| O18 | Sandbox namespace/cgroup feasibility packet, not an implemented isolation feature | experimental | n/a | D | - |
| O19 | Destructive development-startup GC requires explicit opt-in | complete | fork ops only | TD | tests in its PRs: `scripts/dev-env.test.sh` (the earlier file-name check missed them). |
| O20 | Merge-group CI validation and reuse of green merge-group runs | complete | fork ops only | TDI | - |
| O21 | CD approvals per version, superseded builds cancelled, approved deployments preserved, cached/parallel qualification, deploy flag isolation, backend shard | complete | fork ops only | TUDIS | - |
| O22 | Migration prefix-collision gate | complete | fork ops only | TI | - |
| O23 | Upstream-sync guard for source and test losses | complete | fork ops only | TDI | - |
| O24 | Issue-link requirement on PRs and pre-merge security gate | complete | fork ops only | TI | - |
| O25 | Warning-budget normalisation fixes | complete | fork ops only | TI | - |
| O26 | Governance teardown flake fix, stale-verdict race test, web page-load baseline | partial | fork ops only | TI | partly removed from main: 1cb8be9138 (#703, CHE-2760) deleted governance_config_teardown_race_test.go |
| O27 | Fork documentation and process records: feature ledger, built-in skill CLI reference, changelog backfill and link, build-once rule, settings-first rule, active-agents run count | partial | fork ops only | TUDI | reconcile with upstream first (f0c361ebca; 7c186016a5; 354243b459; bb3747186e) |
| O28 | Secret and security scanning: gitleaks, weekly full-tree scan, code-owner scan paths | complete | fork ops only | TI | - |
| O29 | CI cost and structure: X99 caches, main-build split, PR guards consolidated, runner variables | complete | fork ops only | TDI | - |
| O30 | Upstream mirror and upstream PR watcher | complete | fork ops only | TDI | - |
| O31 | Merge-queue stall alarm and dequeue report | complete | fork ops only | TDI | - |
| O32 | CD freshness alarm and evidence-only release-candidate workflow | complete | fork ops only | TDI | - |
| O33 | Web-perf specs: streaming and chip resolution R1-R2, chip retention R3-R4, compact-thread reply R5, scroll-anchor stability | complete | fork ops only | T | - |
| O34 | PR merge automation | complete | fork ops only | TI | - |
| O35 | Process check scripts | complete | fork ops only | TI | - |
| O36 | Fork docs served at the instance `/docs` and in-app links pointed to it | complete | fork ops only | TUDIS | - |
| X01 | Decision quote/DM information-card direction | experimental | n/a | D | - |
| X02 | DM statistics/archive-menu exploration and admit sheets | experimental | n/a | D | - |
| X03 | DM search/notification/Web Push design and amendments | experimental | n/a | UD | - |
| X04 | Bare issue-key chip stability directions | experimental | n/a | UCDS | - |
| X05 | DM follow-up/wakeup-chip directions | experimental | n/a | D | - |
| X06 | Run-mode chip direction C | experimental | n/a | D | - |
| X07 | DM progress card direction C | experimental | n/a | D | - |
| X08 | Issue sidebar compact v2 | experimental | n/a | D | - |
| X09 | Timeline views: Gantt B, Swimlane A, Hierarchy A, Sidebar A | experimental | n/a | D | - |
| X10 | Decision card fold direction A | experimental | n/a | D | - |
| X11 | Workflow lint UI direction D | experimental | n/a | D | - |
| X12 | Delegated governed writes direction B | experimental | n/a | D | - |
| X13 | Decision-bound issue status writes design | experimental | n/a | D | - |
| X14 | Background agents framework spec | experimental | n/a | D | - |
| X15 | Pre-LLM gate plan for no-output runs (CHE-2568) | experimental | n/a | TD | - |
| X16 | DM attention direction B lock | experimental | n/a | D | - |
