# Upstream-readiness audit

Companion to [FORK-FEATURES.md](FORK-FEATURES.md) (CHE-2071). It rates each ledger row and lists what is missing before an upstream PR.

## Method and limits

Static. No runtime qualification and no coverage run. Evidence per row is mechanical, taken from the fork PRs the ledger names:

- **Files** flags show which kinds of file the row's PRs touched: `T` tests, `U` web/desktop UI, `C` CLI, `D` docs, `M` migration, `I` CI/deploy, `S` other server code. `T` means a test file was touched, not that the behavior is covered.
- **complete**: fork-only or fully reconciled, its PRs touched tests, and no parity gap is known.
- **partial**: overlaps an upstream change that must be reconciled first, or its PRs touched no tests, or a CLI/UI/docs parity gap is known.
- **experimental**: design-only record or feasibility packet. **removed**: kept out on purpose.
- **Upstream fit**: `yes` stands alone; `needs design talk` is a subsystem upstream has not asked for; `fork integration` depends on Jev, Hermes or fork-only services; `fork ops only` is CI, deploy or release plumbing for this fork.
- Known-gap notes come from reading the CLI surface and builtin skill, not from reading every handler. A row with no note is not proof of parity.

## Totals

| Rating | Rows |
| --- | --- |
| complete | 61 |
| experimental | 6 |
| partial | 35 |
| removed | 2 |

| Upstream fit | Rows |
| --- | --- |
| fork integration | 6 |
| fork ops only | 21 |
| n/a | 8 |
| needs design talk | 16 |
| yes | 53 |

## Gap list to close before an upstream PR

1. **Reconcile overlaps first.** Every `overlaps` row needs the ledger's merge choice (R01 to R20) applied. Two need a product choice from Cheese: R08 and R09.
2. **CLI parity.** Missing verbs, in the order CHE-2071 sets: workflow create/delete/handoff set; project set-workflow; project and workspace wakeups; status catalog (owner-only); DM archive and pin (`multica chat rename` exists). Also no `chat search` verb (C15) and no CLI for wakeup definitions (W06) or the pool and budget views (W07 to W09).
3. **Builtin-skill docs.** Done. Every registered command is named in the skill or the brief, with no exemption list. `TestEveryCommandIsDocumentedInTheBuiltinSkill` fails on any new command that is not.
4. **Tests.** Rows whose PRs touched no test file are marked partial below; add behavior tests (or point at existing ones) before proposing them.
5. **Split the proposal.** Do not send governance (G01 to G18) or Jev rows upstream as one PR. Offer small, stand-alone features first (comment resolve `known_as_of`, DM fork/continue, decision requests) and ask upstream about the subsystems.

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
| W01 | CLI read/edit of project workflows | partial | yes | TCD | reconcile with upstream first (0beaeddc0 / MUL-8120; da1671ca2 / MUL-7420) |
| W02 | Per-issue workflow override and guarded reassignment | partial | yes | TUCMS | reconcile with upstream first (da1671ca2 / MUL-7420) |
| W03 | `--no-start` preserves a changed handler's workflow handoff | partial | yes | TS | reconcile with upstream first (da1671ca2 / MUL-7420) |
| W04 | Green-check owner wakeup and optional workflow auto-advance | partial | yes | TUMS | reconcile with upstream first (e720fe221 / MUL-8135); `issue_workflow_ci_advance.go` has no sibling test file; needs R02 decision. |
| W05 | Automatic PR merge/failure/attention wakeups, including base-branch and accepted-to-draft changes | partial | yes | TUDMIS | reconcile with upstream first (e720fe221 / MUL-8135) |
| W06 | Built-in definition model, scoped cascade, CRUD/preview and legacy projection/backfill | partial | yes | TUMS | reconcile with upstream first (5ed09342a / MUL-7680); No CLI for wakeup definitions (project/workspace wakeups still missing). |
| W07 | Separate local/default wakeup capacity pools | partial | yes | TUMS | No CLI to see pool usage. |
| W08 | Aggregate wakeup admission budgets | partial | yes | TUMS | No CLI to set aggregate budgets. |
| W09 | Explicit suppress/defer when an agent already has active work | partial | yes | TUMS | No CLI to see the suppress/defer disposition. |
| W10 | Atomic recovery from PR-wakeup rate pause | partial | yes | TUMS | reconcile with upstream first (5ed09342a / MUL-7680) |
| W11 | Workspace-wide wakeup firing pause | complete | yes | TUCDIS | - |
| W12 | Wakeup activity identifies trigger, agent and run | partial | yes | TUDS | reconcile with upstream first (5ed09342a / MUL-7680) |
| W13 | Comment triggers fold into queued work, wake blocked leaders and avoid author self-trigger | complete | yes | TUCDIS | - |
| W14 | Idle squad-leader failure markers | complete | yes | TUCMIS | - |
| H01 | Multi-organization GitHub installations with verified account authority | partial | yes | TUS | reconcile with upstream first (caeb146ba) |
| H02 | Explicit PR close intent, quoted-code stripping and open-child completion gate | partial | yes | TDS | reconcile with upstream first (0da0b46bb / MUL-7672) |
| H03 | Merged PRs advance custom done-category review states | partial | yes | TUS | reconcile with upstream first (fc4a61570 / MUL-7726) |
| H04 | Actor/task-aware automatic PR linking and CLI unlink repair | partial | yes | TUCS | reconcile with upstream first (0da0b46bb / MUL-7672) |
| H05 | Snapshot refresh for already-decided PRs and latest-run-per-name/app check filtering | partial | yes | TUMS | reconcile with upstream first (e720fe221 / MUL-8135; ecce58986 / MUL-5265) |
| H06 | PR close-target validation and non-leaking sparse checkout | partial | fork ops only | UI | no test file in its PRs; Workflow file has no test; CI-only. |
| H07 | Durable merge-announcement writer, subsequently removed | removed | n/a | TUCDMIS | - |
| H08 | CLI browser handoff exchanged for a CLI token | partial | yes | TUDS | reconcile with upstream first (da278c7d4 / MUL-8154) |
| G01 | Field-scoped agent denial for governed instruction writes and the Hermes exception | partial | needs design talk | TDS | No sibling test file for `governed_instruction_fields.go`. |
| G02 | Owner-only digest compare-and-swap for workspace/squad/agent instructions | complete | needs design talk | TUCS | - |
| G03 | Lossless file/stdin squad instruction input | complete | yes | TUCD | - |
| G04 | Two-agent governed-write votes and CLI vote receipts | complete | needs design talk | TUCMS | - |
| G05 | Required source-link enforcement on the server | complete | needs design talk | TCS | - |
| G06 | Deterministic protocol lint, persisted violations, omission reporting and complete waiver lookup | complete | needs design talk | TUDMS | - |
| G07 | Checkpoint-based briefing, reduction and claim-time persistence | complete | needs design talk | TUDMS | - |
| G08 | Typed duplicate admission for issue assignment, rerun lineage and autopilot-created issues | complete | needs design talk | TUCDMS | - |
| G09 | Provider hold across dispatch, manual rerun, retry and claim | complete | needs design talk | TUDS | - |
| G10 | Jev flags, scoped client, state budget, production/no-key gate and missing-mention evaluation | complete | fork integration | TUDIS | - |
| G11 | Jev routing receipts independent of published comments | complete | fork integration | TMS | - |
| G12 | Canonical inherited rule revision resolution | complete | needs design talk | TUS | - |
| G13 | Workspace governance control gate and proposal task credential restriction | complete | needs design talk | TUDMS | - |
| G14 | Immutable cases, fenced lifecycle transitions and expired-evidence refresh | complete | needs design talk | TUMS | - |
| G15 | Case evidence redaction, retention and audited provenance export | complete | needs design talk | TUCDMS | - |
| G16 | Export privacy modes, retention and admin controls | complete | needs design talk | TUMS | - |
| G17 | Atomic case admission and budget reservation across windows, crash recovery and bounded ledger queries | complete | needs design talk | TUCDMIS | - |
| G18 | Bounded observation, rollback deadlines, timeout classification and real-budget test controls | complete | needs design talk | TUCDMS | - |
| G19 | Jev squad-leader routing, shadow mode, error handling and trigger shadow ledger | complete | fork integration | TUCDMS | - |
| G20 | Jev mode/confidence, credential, rollback and API-URL controls | complete | fork integration | TUDS | - |
| G21 | Owner maintenance and conditional agent environment controls | complete | fork integration | TUCDS | - |
| G22 | Credential-exclusive quota-runner execution | complete | fork integration | TUCDS | - |
| G23 | Leader reconciliation proof and durable event-key precedence in briefing | complete | needs design talk | TUDS | - |
| D01 | Atomic daemon single-instance startup and PID publication safety | partial | yes | TUCS | reconcile with upstream first (191c782d6 / MUL-7988) |
| D02 | Fork daemon artifact distribution, server-hosted builds and install-while-busy | partial | fork ops only | TUCDIS | reconcile with upstream first (fcd13aece / MUL-2100) |
| D03 | Per-agent Hermes external-memory opt-in | complete | yes | TUCDS | - |
| D04 | Codex SQLite-init retry, false-negative tool-access handling, forced model catalog and poisoned multi-agent resume avoidance | complete | yes | TUDS | - |
| D05 | AGY managed MCP config and retirement of poisoned executor/malformed-call sessions | complete | yes | TUMS | - |
| D06 | Pre-LLM provider recovery, gateway backoff and retry-pending coordinator suppression | partial | yes | TUDMS | reconcile with upstream first (53c0088f1 / MUL-7847; ebc7c4696) |
| D07 | Oversized leader issue sessions start fresh | complete | yes | TS | - |
| D08 | Ignore repeat delivery of an already executing task | complete | yes | TS | - |
| D09 | Claimed-model readback alongside claim provenance | partial | yes | TUS | reconcile with upstream first (482ec69c1 / MUL-7969) |
| D10 | Agent-skill runtime target setting from the CLI | complete | yes | TC | - |
| D11 | Price model-specific Hermes/Codex usage | partial | yes | TUS | No upstream price-table mapping confirmed (ledger). |
| D12 | Bounded WeCom inbox acceptance wait | complete | fork ops only | TS | - |
| O01 | X99 build/CI ownership, runner migration and shared-Postgres/turbo contention limits | complete | fork ops only | TUDMIS | - |
| O02 | Web build CPU/worker OOM limits | complete | fork ops only | TUD | - |
| O03 | Immutable image qualification and qualified C00 delivery admission | complete | fork ops only | TUDIS | - |
| O04 | D2 migration quiescence and retained-archive rollback safety | complete | fork ops only | TUDMIS | - |
| O05 | Blue/green application slots, router state, atomic cutover and correct rollback tuple | complete | fork ops only | TUDI | - |
| O06 | C00 queue stall alarms and superseded-candidate skipping | partial | fork ops only | UDI | no test file in its PRs |
| O07 | Bounded candidate image-pull retry | complete | fork ops only | TUDI | - |
| O08 | Historical migration identity reconciliation | complete | fork ops only | TUDMIS | - |
| O09 | Scoped Jev environment inputs survive backend recreation | complete | fork ops only | TDI | - |
| O10 | Integration tests fail without their database and avoid shared-fixture leakage | complete | fork ops only | TUDMIS | - |
| O11 | Real-browser disclosure regressions and repaired upstream-sync E2E selectors/timeouts | complete | fork ops only | TUD | - |
| O12 | Go/network dependency security update | partial | fork ops only | UDS | no test file in its PRs; reconcile with upstream first (ce5a2ba47 / upstream #9420) |
| O13 | Isolated paired-replay instrumentation | partial | fork ops only | UD | no test file in its PRs |
| O14 | Fork build stamps and distinct server version/build/commit fields | complete | fork ops only | TUCDIS | - |
| O15 | Fork changelog line contract, generation, preview and qualified release display | complete | fork ops only | TUDIS | - |
| O16 | Autopilot cooldown lookup scope and unset probe wiring | complete | fork ops only | TUCS | - |
| O17 | Fork CLI/skill reference and child completion briefing updates | complete | fork ops only | TUDS | - |
| O18 | Sandbox namespace/cgroup feasibility packet, not an implemented isolation feature | experimental | n/a | D | - |
| O19 | Destructive development-startup GC requires explicit opt-in | partial | fork ops only | D | no test file in its PRs; Startup script, no test touched. |
| X01 | Decision quote/DM information-card direction | experimental | n/a | D | - |
| X02 | DM statistics/archive-menu exploration and admit sheets | experimental | n/a | D | - |
| X03 | DM search/notification/Web Push design and amendments | experimental | n/a | UD | - |
| X04 | Bare issue-key chip stability directions | experimental | n/a | UCDS | - |
| X05 | DM follow-up/wakeup-chip directions | experimental | n/a | D | - |
