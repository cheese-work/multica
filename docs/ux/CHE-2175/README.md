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
The leader later captured the authenticated full-route pack on
`7ef9664dd0a5153094558299ac8da2f93ecd7c67`, in CHE-2175 comment
`01a17c44-6645-7012-aa24-60d54048eeec`. Both widths show card 2 answered and
card 1 open after sending. The runtime was offline, so those captures do not
exercise answer withdrawal. B2 changes no UI source and claims no new captures.
The PR stays draft until the required independent exact-head review is complete.

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
B2 supersedes B1's binding-clear mechanism with option 1. The existing
failed/cancelled-turn withdrawal rule remains unchanged. Withdrawal keeps the
user-message binding to its withdrawn answer, preserving the explicitly
selected request. Retry context names that request, and source reuse refuses
any other request. Re-recording on the selected request replaces the binding
with the live answer atomically and still applies the decision hold.
The workspace-wide unbind query is removed (N5); re-binding targets the exact
message ID and compares the previous answer ID. Human disclaim and agent veto
also retain the selected-request binding (N6). Existing historical
decision-reference messages remain intact.
B3 treats a missing referenced answer as no binding, including after issue
deletion and decision orphan cleanup. Chat decision context and decision
recording then use the existing unbound-message path. Other database errors
still propagate. B2's selected-request checks remain unchanged when the answer
row exists, whether live or withdrawn. B3 changes no UI source or captures.
The bound send path refuses `/compact` commands without creating any rows.
DM quotes use the existing plain-quote serializer; issue annotation escaping is
unchanged.
