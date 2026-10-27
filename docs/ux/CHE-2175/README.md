# Decision-request annotation evidence

## Scope

CHE-2175 reuses the CHE-1876 annotation popup, markers, chips and agent-reply
accessibility label. No new design is introduced.

- Design: `design/dm-agent-message-annotation/rev-2/A.html` from PR 312.
- Approved merge: `283b80a1a7b27cfa95ab211f3dfec7b2ec2770fa`.
- Admit sheet: `docs/ux/CHE-2175.yml`.
- Before implementation: `080322d5bb55436c0e07b682ac0b9ace59c21a72`.
- Preview: not available.
- Visual diff: NOT-RUN (CHE-1485 Layer B pending).

## X99 browser captures

These are component-fixture captures, not an authenticated application session.
The fixture renders the real `DecisionRequestCard`, annotation hook, popup,
markers and chip, with two synthetic open asks and a simple composer shell.
The before fixture serves the card implementation directly from the base SHA.
The after fixture uses this branch's implementation. Both widths were checked
for the expected revision, absence of page errors, and visible card content.
The temporary fixture and capture server were removed after capture.

The resting before and after PNGs are byte-identical at each width. This is
expected: the change adds annotation behavior and accessibility metadata, not
new resting visuals. The popup, marker and chip appear only after interaction.
The identical PNGs do not prove full-route behavior or a visual change.

SHA-256 for each before/after pair:

```text
1440: d482b1ab7a87281551eb782ce6fcca9239799bf8544df65aeadd902100d4d521
390:  26c4a5147f1f95870e2f523b1066052e1fbd627f56b0f07c2db514f06936a459
```

| Surface | Width | Before | After | Add note | Saved annotation |
| --- | --- | --- | --- | --- | --- |
| Decision cards | 1440 | [Before](chat-1440-before.png) | [After](chat-1440-after.png) | [Popup](chat-1440-note.png) | [Marker and chip](chat-1440-annotated.png) |
| Decision cards | 390 | [Before](chat-390-before.png) | [After](chat-390-after.png) | [Popup](chat-390-note.png) | [Marker and chip](chat-390-annotated.png) |
| Authenticated `/<workspace-slug>/chat` | 1440 | NOT-CAPTURED | NOT-CAPTURED | Not exercised | Not exercised |
| Authenticated `/<workspace-slug>/chat` | 390 | NOT-CAPTURED | NOT-CAPTURED | Not exercised | Not exercised |

The full-route capture is not available from this isolated fixture. The fixture
does not exercise authentication, the production composer, realtime refresh or
the network send. Unit and database-backed handler tests cover request binding.
The full-route UI review pack remains incomplete; the PR stays draft until the
required full-route captures and independent exact-head review are complete.

## Keep / Change / Remove

Manual fixture check:

| Element | Result |
| --- | --- |
| `decision-request-card` | Kept: two cards and state labels remain visible. |
| `decision-request-source-issue` | Kept: the issue link remains visible on each card. |
| `decision-request-source-comment` | Kept: the source-comment link remains visible on each card. |
| `chat-annotation-chip` | Kept: the existing chip appears after saving the note. |
| `chat-send-button` | Fixture shell only; the production send button is unchanged, but full-route verification is NOT-RUN. |
| `decision-request-ask` | Changed: open ask supports the existing popup and marker; tests verify the named tab stop and non-annotatable answered/withdrawn states. |
| Unlisted removals | None in the fixture or scoped source diff. |

## Binding contract

One annotated reply answers one explicitly selected request. Notes from another
request or an agent reply cannot join that reply. The DM send includes
`decision_request_id`; the server validates the original interactive human, DM,
agent, workspace, active issue and open asked request before writing anything.

The message, task, answer and issue quote commit together. Existing decision
hold and turn-completion rules still apply. The source message is marked with
its answer ID so the remaining open request cannot consume the same reply.
Option (b) preserves the existing failed/cancelled-turn withdrawal rule.
Withdrawal clears the user-message binding in the same transaction, but keeps
the historical decision-reference message. Retry context and source reuse
check for a live answer, so a stale or withdrawn binding does not prevent
recording the reopened request. Re-recording still applies the decision hold.
The bound send path refuses `/compact` commands without creating any rows.
DM quotes use the existing plain-quote serializer; issue annotation escaping is
unchanged.
