# Agent decision instances (C1)

C1 records proposals and signatures. It creates no settlement, changes no gate,
and supplies no human authorization. Human decision consumers require
`decision_request.authority = 'human'`.

Only a running agent task of the same issue may post a protocol action. Post one
action per comment. Instance UUIDs are canonical lowercase UUIDs and are globally
single-use. Digests are 64 lowercase hex characters. Every action is serialized
under the instance advisory lock.

## Proposal

The proposer must be the work author. The full comment and its SHA256 are frozen,
along with the issue, casting task, provider/model/lab, evidence, signer identities,
D3 delegation and allowlist version. Neither edits nor deletions change the record.

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
authority. Veto and settlement are outside C1.

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
reviewer requires BLOCK. C1 records the verdict only.
