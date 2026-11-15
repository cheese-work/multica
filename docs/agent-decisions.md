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

Allowlist version 2 adds `test_first_red`: a waiver of the test-first red step. Its
record is WAIVED, never PASS. The proposal also carries `reviewer_agent_id` (one of the
two signers, from a different lab than the author), `red_impossible_reason`,
`weaker_evidence_kind` (`characterization_test`, `manual_repro`, `static_check` or
`review_only`) and `followup_check`. Only the cosign route is allowed. The reviewer's
opinion repeats the reason in a `red_impossible_reason` header, which must match the
proposal. A version 1 record stays valid and never gains the new gate.

Every `test_first_red` waiver owes its follow-up check. When the record becomes
effective, the same transaction creates one wakeup on the waived issue (a timed,
one-shot `issue_wakeup` for the author, 7 days after the record took effect). Its
instruction carries the reason, the weaker evidence and the proposal's `followup_check`,
and asks for one comment starting `Follow-up result (<decision instance>):` on the waived issue. A failed
check or a worse crash rate reopens the work (`in_progress`, new PR). The wakeup is
marked with `followup_instance_id` (unique per record), so closing the issue does not
disable it: the check is meant to run after release. A proposal may add
`crash_signature` (a Crashlytics issue id: 1-128 letters, digits, `.`, `:`, `_`, `-`);
the instruction then tells the author to read the Crashlytics crash rate for that
signature after the release (firebase-crashlytics MCP). A closed issue accepts no new
wakeup, so a crash-fix check is not one-shot: it repeats every 72 hours, with no run cap,
until the result is recorded. The result comment starts with
`Follow-up result (<decision instance>):`, posted by the agent that runs the check from a run
this wakeup dispatched; the platform then disables that waiver's wakeup before the next run.
A marker from a member, another agent, an unrelated run of the same agent or no run ends
nothing. Another waiver's result on the same issue does not end this
check. A run that finds the fix unreleased posts nothing. If the change will never ship,
the author records that as the result.
`followup_check` is limited to 2000 bytes. A refused or unsettled proposal creates no
wakeup.

Waiver candidate for a PR (CHE-4326): an agent decision (Jev, once live) uses the PR's own diff digest as
`candidate_sha256`. Print it with this command in a code block:

```sh
python3 scripts/security-gate/security_gate.py --github <owner/repo> --pr <n> --default-branch main --print-digest
```

The server matches the candidate exactly. A main-only merge (no change to any PR file) keeps the same digest, so the
lookup still finds the record. Any change to a PR file gives a new digest and the lookup reports `not decided`. A
candidate that is a head SHA instead voids the record on every head change. A record here is never human
authorization: while Jev is not live, Cheese decides and the waiver is a human decision record (see
`decision-records.md`, "Waiver scope line").

Every new proposal must carry `decision_class: "reversible_gate"`, the only in-scope
class. A missing or unrecognised class is refused (fail closed, no stop rule). A class of
`irreversible`, `design_direction`, `architecture` or `high_level` is refused as an
excluded category and trips the stop rule, for any decider. Records made before this
check keep their stored scope.

A proposal may set `decider: "Jev for Cheese"` with a `confidence` in (0, 1]. It is a
label on an agent decision: the brief shows it, and no answer from Cheese is created.
The label is only accepted from the agent registered in the workspace setting
`agent_decisions_jev` (`{"agent_id", "mode"}`), checked against the task-verified
proposer. Without that setting, with another agent, or with a mode other than `shadow`
or `live`, the proposal is refused. In `shadow` mode the record is kept but never
settles, even with `agent_decisions_enabled` on. In `live` mode it settles under the
same rules as any agent decision. The owner sets it with `PUT /api/agent-decisions/jev`
(`{"agent_id": "<uuid>", "mode": "shadow"|"live"|"off"}`; human owner only, task tokens
get 403). The generic workspace settings save (`PATCH`/`PUT /api/workspaces/{id}`) drops this key and keeps the stored value, so it cannot register, change or erase Jev. Moving `shadow` to `live` is the owner's call after the 7-day shadow and
qualification. Settling an already-recorded Jev decision re-reads the mode, but reliance
on a record that has already settled is not re-gated if the mode later returns to `shadow`.

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
on complete lines. The reviewer body begins `Verdict: BLOCK` and carries typed
header lines (`Key: value`, up to the first line that is not one): `Gate:` must be
on the allowlist and `Block-category:` must be `reversible_gate`. Any other,
missing, unknown or repeated value is excluded and fails closed. Prose is never
read, in any language: a word such as "deploy" in the text neither excludes the
decision nor turns the setting off. Co-signer splits cannot be adjudicated.

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
Veto: decision:<instance uuid> <candidate sha256> <agent|member>:<undo owner uuid>

<reason, 1 to 4096 bytes>
```

Allowed vetoers: a named signer of the instance, the issue's assignee agent, the
leader or a member of the non-archived assignee squad, and a workspace `owner`
member (no task token needed). Anyone else gets 403. The digest must match the
bound candidate. A veto needs a reason and an undo owner: an unarchived agent of
the workspace (`agent:<uuid>`) or a workspace member (`member:<user uuid>`). An
unknown owner is refused with 400. A veto does not depend on current policy: a
record made effective under an older allowlist, delegation or gate list stays
vetoable. Settlement and reliance still check the current policy.

Under the instance advisory lock a veto revokes the instance:

- Before the instance is effective, it inserts a revoked settlement row. The later
  quorum insert hits `ON CONFLICT DO NOTHING`, so a veto and the second reveal
  end in one consistent state: no live settlement.
- After it is effective, it sets `revoked_at` and `revoked_by` once. The settlement
  trigger allows only that NULL-to-value update; a revoked instance never becomes
  effective again.
- Each reliance already registered gets one `decision_agent_undo` row. The call
  that revokes also queues one `agent_decision_undo` inbox item, `action_required`,
  for the undo owner. Its `details` list every reliance (id, task, agent, purpose)
  and the evidence, candidate digest and gate to reverify. The item is queued
  with no reliance too, so the owner confirms nothing leaned on the record.
  A reliance that loses the race to a veto is refused (lookup shows `revoked`);
  one that wins is in the list. The owner is stored on the revoked settlement.

A revoked instance refuses every further protocol action. `Retract:` is refused
once the instance has a settlement row.

Supersession: a proposal may set `supersedes: "<instance uuid>"` to revoke the
author's own earlier instance for the same issue and gate. The reason recorded is
`superseded by decision:<new instance>`. The superseding author owns the undo.

## Pilot and brief (C4)

Setting: `workspace.settings.agent_decisions_enabled`, default off, owner-only
and human-only (`PUT /api/agent-decisions/setting`, `multica decision
agent-setting`). A task token or cloud PAT gets 403, even for the workspace
owner's user. Enabling it on C00 is a separate step. With it
off, the vote path never inserts a settlement. Turning it on clears an earlier
stop.

Stop rule: one excluded-category decision turns the setting off and stores
`settings.agent_decisions_stopped {reason, at}`. Triggers: a proposal whose gate
is outside the allowlist, an adjudication BLOCK whose typed `Gate:` or
`Block-category:` is excluded, missing or unknown, and a
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

Brief output: `multica decision brief --format markdown` prints the section
"Agents decided — veto?" as standalone markdown, outside Needs you, with review
targets in ICT. Each record lists the issue, gate, state, the record link and the
veto line. A revoked record shows its reason. Notice delivery failures and the
stop reason follow. `link` opens the instance's frozen proposal comment, not only
the issue. `veto_line` already ends in `member:<reader's user uuid>`, so the
reader is the undo owner when they paste it.
With the setting off and nothing to show (no record, no stop, no failed notice)
the command prints nothing, so the brief omits the section. Records made
earlier stay vetoable, so with any of them the section still prints.

Waiver and Jev metrics (CHE-3262, slice 6 of CHE-3239): the feed adds `waivers`
(live `test_first_red` records per agent; a waiver is WAIVED, never PASS), `jev`
(`decisions`, `vetoed`, `veto_rate`, `excluded_class_decisions`; the last must be 0
because an excluded class is refused at propose time) and `jev_asks`. The server
cannot read the Jev shadow log, so `multica decision brief --jev-shadow-log
<jsonl>` reads it, sends the Jev-routed ask ids as repeated `jev_ask` query
parameters (at most 1000) and gets back each ask's `created_at` and Cheese's
chosen option label (empty while open). The CLI then prints shadow agreement with
Cheese (answered Jev-routed asks where `pick` equals Cheese's choice), the share
of in-scope asks (any row whose reason is not `excluded`) that Jev decided within
10 minutes of the ask, and whether the live-mode gate holds: agreement of at
least 90% on at least 20 asks. Without the flag the section says the log was not
given. With `--format json` the same figures are under `jev_shadow`.

The daily brief is a Multica autopilot (6f676b71, "Cheese evening brief"). Its
prompt change, "run `multica decision brief --format markdown` and paste its
output after `## Cần anh`", is a standing-instruction change and goes through
the instruction-change process: cheese-work/multica-dotfiles#406 holds the
candidate, the digests and a digest-guarded apply script. Until Cheese
authorizes that apply, the brief does not print the section by itself.

Notice: a settlement writes one `decision_agent_notice` row in its own
transaction. The decision worker delivers it after commit, in one transaction,
to every workspace owner:

- an `agent_decision_notice` inbox item with the record link and that owner's
  veto line. It carries `decision_request_id`, so it ignores the
  `unassigned_activity=muted` preference like any decision;
- a copy in the owner's decision thread for the issue's topic (CHE-2407), created
  when missing, as message kind `agent_decision`. A thread the owner cannot use is
  skipped; the inbox item is the reliable channel.

The notice counts as delivered only when every owner's inbox item was inserted.
Otherwise the transaction rolls back, `attempts` and `last_error` are stored, the
notice retries with backoff (30 s doubling to 1 hour) and shows in
`notice_failures`.

Pilot metrics:

| Field | Meaning |
| --- | --- |
| `asks_avoided` | Live records, deduplicated by issue, gate and candidate hash |
| `vetoed` / `denominator` | Revoked settlement rows over all settlement rows. A veto before effect leaves a revoked row and counts as a veto |
| `runs` / `run_tokens` | Distinct runs that wrote a protocol event (proposal, commit, reveal, veto) and their input plus output tokens |
