# Agent decision instances (C1)

C1 records proposals and signatures. It creates no settlement, changes no gate,
and supplies no human authorization. Human decision consumers require
`decision_request.authority = 'human'`.

Only a running agent task of the same issue may post a protocol action. Post one
action per comment. Instance UUIDs are canonical lowercase UUIDs and are globally
single-use. Digests are 64 lowercase hex characters. Every action is serialized
under the instance advisory lock.

Each workspace and issue permits one instance per candidate digest and gate.
A new UUID cannot replace the original instance or its named signers.

## Proposal

The proposer must be the work author. The full comment and its SHA256 are frozen,
along with the issue, casting task, provider/model/lab, evidence, signer identities,
D3 delegation and allowlist version. Neither edits nor deletions change the record.

Workspace teardown may delete these records under the transaction-local
`multica.workspace_teardown` flag. The decision worker removes orphan scopes and
requests after their issue disappears. Updates remain forbidden even in teardown
mode.

````text
Proposal: decision:<instance-uuid> <candidate-sha256>

```decision
{
  "work_author_id": "<proposer-agent-uuid>",
  "waived_gate": "screenshot_evidence",
  "route": "cosign",
  "signers": ["<signer-agent-uuid>", "<other-signer-agent-uuid>"],
  "evidence": [{"url": "https://example.com/evidence", "sha256": "<evidence-sha256>"}]
}
```
````

Allowlist version 1 has exactly `screenshot_evidence`, `design_lock_label` and
`unrelated_base_failing_check`. The last gate also requires `base_sha` (40 lowercase
hex) and identical, nonempty `base_failed_checks` and `candidate_failed_checks`
arrays of distinct check names. These are attested evidence references; C1 does
not fetch or independently verify remote evidence.

The co-sign route freezes two distinct non-author agents of known, different
labs. At least one lab differs from the author's lab. The casting task's lab must
match the frozen signer lab. No signer replacement is supported.

## Commit and reveal

Each signer first posts only:

```text
Commit: decision:<instance-uuid> <candidate-sha256> <opinion-sha256>
```

After every named signer commits, each signer posts:

````text
Vote: PASS decision:<instance-uuid> <candidate-sha256>

```opinion
decision_instance: <instance-uuid>
candidate_sha256: <candidate-sha256>
waived_gate: screenshot_evidence
signer_agent_id: <signer-agent-uuid>
proposal_sha256: <full-proposal-body-sha256>
evidence: [{"url":"https://example.com/evidence","sha256":"<evidence-sha256>"}]
verdict: PASS
independence: formed before reading any other signer's opinion
nonce: <32-random-lowercase-hex-characters>

Reasoned opinion.
```
````

Hash the UTF-8 opinion bytes after CRLF-to-LF normalization, without fence lines,
including the final newline. Use exactly one triple-backtick `opinion` block.
Each header field appears once. All bindings must match the frozen proposal and
trusted signer. Evidence items must match in the same order. `BLOCK` is also a
verdict; the Vote line must equal the committed verdict. A second initial vote
is refused. The older governed-field BLOCK-as-retraction behavior is unchanged.

A valid reveal before the other commitment returns HTTP 400, creates no comment,
and persists a full-body `void` audit with its casting task. Its allocated comment
ID identifies the rejected attempt, not a visible comment. That signer cannot
retry. Other malformed reveals are refused without creating a comment.

```text
Retract: decision:<instance-uuid>
```

Retraction after a commitment permanently removes that signer's vote from the
`agent_decision_live_vote` tally input. Neither the view nor a recorded PASS grants
authority. Veto and settlement are outside C1 (see C3 below).

## Adjudication

Use `route: "adjudicate"`, omit `signers`, and provide an `adjudication` object:
`reviewer_comment_id`, `reviewer_task_id`, `reviewer_verdict`, `author_comment_id`,
`author_task_id`, `author_rebuttal`, `point` and `adjudicator_agent_id`.

The verdict and rebuttal must equal the original, unedited task-authored comments
on this issue. Both bodies bind `Disputed-point: <point>` and `Gate: <waived_gate>`
on complete lines. The reviewer body begins `Verdict: BLOCK` and includes
`Block-category: reversible_gate`. Security, excluded gates, missing evidence,
protected gates and co-signer splits cannot be adjudicated.

The one adjudicator must differ from both disputants and their labs. Its frozen
opinion uses the same commit/reveal protocol, plus `disputed_point: <point>` and
`upholds: author|reviewer`. Upholding the author requires PASS; upholding the
reviewer requires BLOCK. C3 settles it: PASS with `upholds: author` from the one adjudicator permits the
same single settlement insert as a co-sign quorum.

## Settlement and reliance (C2)

Settlement creation is gated by the workspace setting
`agent_decisions_enabled` (see Pilot and brief (C4)). It defaults to off.

The co-sign engine runs after the vote snapshot in the comment-insert
transaction, under the instance advisory lock. Two valid named PASS reveals with
no void or retraction permit one insert. The instance primary key and
`ON CONFLICT DO NOTHING` prevent duplicates. Any insert failure rolls back the
whole transaction, including the vote comment. Settlement and reliance history
are immutable; the decision worker removes orphan history after issue deletion.

`GET /api/issues/{id}/agent-decisions?gate=<gate>&candidate_sha256=<sha256>`
shows the bound instance state. It always returns `authorization: false` and
`decided: false`, including when the display state is `effective`. It never
registers reliance.

`POST /api/issues/{id}/agent-decisions/rely` accepts
`{ "gate": "screenshot_evidence", "candidate_sha256": "<sha256>", "purpose": "<work>" }`.
It requires a running task token for the same issue. The server derives task and
agent IDs from the authenticated token. Under the same instance lock used by
comments and the future veto path, it checks agents authority, issue, gate,
digest, allowlist version, delegation, effective time and revocation, then inserts
one reliance row. Only a successful commit returns `decided: true` with instance
and reliance IDs. Validation, insert, lock or commit failures never authorize.

CLI equivalent:

```sh
multica decision rely --issue CHE-2643 --gate screenshot_evidence \
  --candidate-sha256 <sha256> --purpose "Review screenshot gate"
```

Read the JSON `decided` field. Exit status 0 alone does not mean a gate is waived.
Human merge, deployment and excluded gates still require their own authority.

## Veto, revocation and undo (C3)

A veto is a protocol comment on the instance thread:

```
Veto: decision:<instance uuid> <candidate sha256>

<reason, 1 to 4096 bytes>
```

Allowed vetoers: a named signer of the instance, the issue's assignee agent, the
leader or a member of the non-archived assignee squad, and a workspace `owner`
member (no task token needed). Anyone else gets 403. The digest must match the
bound candidate. A veto needs a reason.

Under the instance advisory lock a veto revokes the instance:

- Before the instance is effective, it inserts a revoked settlement row. The later
  quorum insert hits `ON CONFLICT DO NOTHING`, so a veto and the second reveal
  end in one consistent state: no live settlement.
- After it is effective, it sets `revoked_at` and `revoked_by` once. The settlement
  trigger allows only that NULL-to-value update; a revoked instance never becomes
  effective again.
- Each reliance already registered gets one `decision_agent_undo` row and an
  `agent_decision_undo` inbox item for the relying agent. A reliance that loses the
  race to a veto is refused (lookup shows `revoked`); one that wins is listed in
  the undo assignment.

A revoked instance refuses every further protocol action. `Retract:` is refused
once the instance has a settlement row.

Supersession: a proposal may set `supersedes: "<instance uuid>"` to revoke the
author's own earlier instance for the same issue and gate. The reason recorded is
`superseded by decision:<new instance>`.

## Pilot and brief (C4)

Setting: `workspace.settings.agent_decisions_enabled`, default off, owner-only
(`PUT /api/agent-decisions/setting`, `multica decision agent-setting`). With it
off, the vote path never inserts a settlement. Turning it on clears an earlier
stop.

Stop rule: one excluded-category decision turns the setting off and stores
`settings.agent_decisions_stopped {reason, at}`. Triggers: a proposal whose gate
is outside the allowlist, an adjudication BLOCK in an excluded category, and a
stored record whose gate left the allowlist reaching settlement. Existing
records stay vetoable because veto never reads the setting. Not covered: a
reliance on a record whose gate later left the allowlist is refused and does not
change the setting.

Brief feed: `GET /api/agent-decisions/brief` (workspace member). Fields:
`decisions` (live records plus revoked ones from the last 7 days, each with
`link`, `veto_line`, `review_target_at` = effective + 24 hours), `notice_failures`,
`pilot`, `enabled`, `stopped`. The review target is shown to Cheese as a target.
It is not an expiry: a veto works at any time. Agent records never enter Needs
you; they only appear in this feed.

Brief producer: the daily brief is a Multica autopilot owned by agent
x99-claude-opus. It prints the section "Agents decided — veto?" from
`multica decision brief`. Editing that autopilot prompt is a separate workspace
instruction change, not part of this code.

Notice: a settlement writes one `decision_agent_notice` row in its own
transaction. The decision worker delivers it after commit as an
`agent_decision_notice` inbox item to every workspace owner, with the issue link
and the veto line. A failed delivery stores `attempts` and `last_error`, retries
with backoff (30 s doubling to 1 hour) and shows in `notice_failures`.

Pilot metrics:

| Field | Meaning |
| --- | --- |
| `asks_avoided` | Live records, deduplicated by issue, gate and candidate hash |
| `vetoed` / `denominator` | Revoked settlement rows over all settlement rows. A veto before effect leaves a revoked row and counts as a veto |
| `runs` / `run_tokens` | Distinct runs that wrote a protocol event (proposal, commit, reveal, veto) and their input plus output tokens |
