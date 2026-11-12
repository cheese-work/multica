# System 1: shadow capture and integration plan

System 1 is a fast predictor of routing and decision answers. It learns from
past decisions, as Jev does. Cheese decided (CHE-3241, 5 Nov 2026 ICT):

- Collect the data now. Do not fine-tune yet.
- Run in **shadow mode** first. A prediction is stored and never applied.
- The recommended option **stays first** in the UI.

This page covers what is captured, how the shadow predictor works, where
System 1 plugs in later, and the criteria for each later step.

## What is captured

One row per event in `system1_observation` (migrations `936` to `939`).

| Field | Routing row | Decision row |
| --- | --- | --- |
| `kind` | `routing` | `decision` |
| `subject_id` | Issue id | Decision request id |
| `class` | `<workflow>/<step status>` | `escalation_class` (for example `reversible`) |
| `decision_source` | `rule` (a workflow handoff chose the handler) | `human` once answered. Empty while open. |
| `inputs` | Issue status, project, workflow | Origin, issue status, project, asking agent, option count |
| `option_count` | Not used (0) | Number of options offered. No labels. |
| `recommended_index` | Not used | Position of the recommended option as shown. `0` after the first-position rule. Empty when the ask did not come through the request API. |
| `agent_confidence` | Not used | The agent's own confidence, 0 to 1. Optional ask field `confidence`. |
| `predicted_key`, `predicted_confidence`, `predicted_precedents`, `predicted_at` | Shadow prediction | Shadow prediction, stored in the same transaction as the request |
| `choice_key`, `chosen_index`, `answered_at`, `time_to_answer_ms` | Chosen handler (`agent:<id>` or `member:<id>`) | The human answer. `choice_key` is `recommended`, `other`, `option` or `free_text`. |
| `reversed_at` | A person reassigned the issue within 1 hour | The answer was disclaimed |

### Route label (CHE-4439 S1)

Kind `route` is the routing agent's own pick, written in the issue write's
transaction. `class` is `<workflow>/<from status>`, `choice_key` is
`<to status>|<owner type>:<owner id>`, `decision_source` is `agent` or `human`,
and `inputs.via` is `route` (`issue route`) or `status` (the fallback).
`event_id` is the id of the `status_changed` activity row the label belongs to.
An owner-only route changes no status and keeps `event_id` NULL.
`inputs.routing_exit` is false for a route from a step with no routing agent.

### Test-first waiver ask (CHE-4383 B2a)

A waiver ask is an ordinary decision ask with a closed contract. System 1
reads the typed fields, never prose. The ask file carries:

- `topic`: `test_first_waiver`, the only value. The CLI flag
  `multica decision ask --topic test_first_waiver` must equal it.
- `waiver`: `repo` (`owner/name`), `head_sha` (40 lowercase hex),
  `red_impossible_category` (`no_behavior_change`, `missing_test_harness`,
  `historical_commit`, `external_dependency`, `other`), `weaker_evidence_kind`
  (`characterization_test`, `manual_repro`, `static_check`, `review_only`),
  `cross_lab_pass_on_head` and `tests_retrospective` (both booleans, both
  required) and `change_kind` (`docs`, `tests_only`, `dependency`,
  `new_file`, `code`).
- `options[].option_key`: exactly two options, one `waive` and one `refuse`.

The server refuses with HTTP 400 any other topic, a `waiver` block or an
`option_key` without the topic, an unknown or missing waiver field, a bad
value, or any option set other than `waive` plus `refuse`. Nothing is stored
on refusal.

The decision row's `inputs` then also holds `topic`, `waiver` and
`option_keys`. `option_keys` follows the stored order, after the server moves
the recommended option first. So `option_keys[0]` is the recommended key.
No migration: these live in the existing `inputs` jsonb.

How it is written:

- **Decision, ask side:** `recordDecisionObservation` runs in the request
  transaction. The prediction exists before any answer can.
- **Decision, answer side:** database triggers on `decision_answer`. Every
  answer path (card, DM, agent record) is covered without handler edits. A
  second trigger records a disclaimed answer as a reversal.
- **Routing:** `recordSystem1Routing` runs after an issue update that changed
  the assignee. Failure is logged and never fails the update.

Not captured: the original `decision_source = agent` case. No code path lets an
agent decide a request today, so the value exists in the schema and has no
writer.

### Waiver labels (CHE-4383 B2b)

Table `system1_label` (migration `99951_system1_label`, no foreign key, no
free text): `observation_id`, `scope`, `kind` (`burn_in`, `eval`), `source`
(`human_answer`), `label`, `superseded_at`.

- **Answer side:** the `decision_answer` insert trigger reads the stored
  `inputs.option_keys` of an observation with topic `test_first_waiver` and
  writes one label for the chosen index: `waive` or `refuse`, whatever the
  option order. A free-text answer (no index), or a key outside
  `waive`/`refuse`, is `unmapped`. `unmapped` stays out of the agreement
  denominator and is counted separately.
- **Disclaim:** the disclaim trigger sets `superseded_at` on the answer's
  label, beside the existing `reversed_at`. A superseded row is never the
  label of record.
- **Withdraw then re-answer:** a replacement answer on the reopened request
  writes its own label (the old one stays superseded), because no live label
  remains. The observation row keeps the first answer's choice fields.
- An ask without the topic gets no label row; its `choice_key` stays
  positional.
- Later slices add labeller rows (`source`, task id, correction link) and
  the lookup index when a query needs it.

### Waiver predictions (CHE-4383 B3)

Table `system1_prediction` (migration `99968_system1_prediction`, no foreign
key): `observation_id`, `scope`, `predictor_id`, `predictor_version`,
`predicted_key` (NULL = abstained), `predicted_confidence`,
`predicted_precedents`, `predicted_at`. One row per observation, predictor and
version (unique). The version is part of the identity; a changed rule is a new
version, never an edit.

- **Written in the ask transaction**, right after the observation, so both rows
  exist before any answer (`predicted_at` precedes `answered_at`). Only a
  waiver ask (topic `test_first_waiver`) writes them; the legacy
  `system1_observation.predicted_*` columns stay as they were.
- **`waiver_rule@1`:** `refuse` when `cross_lab_pass_on_head` is false; `waive`
  when it is true and `weaker_evidence_kind` is not `review_only`; abstains
  otherwise.
- **`precedent@1`:** `system1.Predict` over the earlier `waive`/`refuse`
  labels of record in the workspace (newest 200, not superseded, `unmapped`
  excluded). Fewer than 5 precedents abstain.
- `jev@<version>` is not written: Jev is shadow only and needs B0's finding
  that it is available.
- Later slices add the epoch, the case rules and the report over these rows
  (B5), and `ci_rules@<version>` rows for CI scopes (B6).

## Recommended option stays first

- The request API (`RequireRecommendedFirst`) moves the recommended option to
  index 0 and stores it that way. The ask names it in the field
  **`recommended_option`**: the exact option label, case-insensitive.
- The `recommendation` prose is **never read** to pick the option. Free text
  cannot be matched reliably ("Proceed is risky; I recommend Hold", "Hold
  rather than Proceed"), and a wrong guess puts the wrong option first and
  flips the agreement signal.
- An ask with two or more options is refused with a 400 when
  `recommended_option` is missing, blank or matches no label. So no stored ask
  breaks the first-position rule.
- The shadow capture stores the index of that option as stored, so
  `recommended_index` is always `0` for requests made through the API. Asks
  with fewer than two options (actions) are not affected. Asks the server
  creates itself (for example run-budget decisions) do not pass this gate and
  record an empty `recommended_index`.
- Why it matters: Cheese picked the recommended option 92.1% of the time while
  the recommendation was first 92% of the time. Position and recommendation are
  confounded. A model trained on this cannot tell "the recommendation was good"
  from "it was first".

### Agreement where the recommendation was not chosen

The report separates the slice where the human chose `other` or `free_text`.
Agreement there is the honest signal. Agreement on the recommended slice is
inflated by position. The report returns both.

A **blind audit sample** (a random share of requests shown in shuffled order)
is **not built**. Add it only if the "other" slice stays too small to judge
(see fine-tune criteria).

## Shadow predictor

`system1.Predict(precedents []string) Prediction` in `server/internal/system1`.

- Precedents: the 200 most recent non-reversed `choice_key` values in the same
  workspace, kind and class.
- Prediction: the majority key. Confidence is `(count+1)/(n+2)`.
- Abstains (empty key) with fewer than 5 precedents or a tie.
- Never applied. No code reads `predicted_key` to act.

### Report

`GET /api/decisions/shadow-report` (workspace member).

CLI: `multica decision shadow-report` (JSON output; any workspace member).

- Per kind and class: decided, predicted, agreed, the same three for the
  "other" slice, reversed, median time to answer.
- `ready` is true once a kind has 100 decided rows. Report precision and
  coverage by class at that point.
- Precision = agreed / predicted. Coverage = predicted / decided.

#### Route block (CHE-4439 S6)

`since` and `until` (RFC 3339; default: the last 24 hours, `until` defaults to
now) set the window `[since, until)`. They apply to the `route` block only.
The `decision` and `routing` blocks stay all-time. A bad time or `since` after
`until` is a 400. The CLI takes `--since` and `--until`.

Coverage and adoption use one event population, joined by `event_id`:

- `eligible_exits`: agent `status_changed` rows in the window with
  `details.routing_exit = true`. The mark is written at apply time, so a later
  setting change does not move past numbers.
- `matched_labels`: eligible events with at least one route row with
  `decision_source = agent`. `label_coverage = matched_labels / eligible_exits`,
  at most 1. A fallback capture failure is an eligible exit with no match.
- `via_route_share`: matched events whose earliest agent route row has
  `via = route`, divided by `matched_labels`.
- `duplicate_labels`: agent route rows in the window that share a non-null
  `event_id` with an earlier row. Target 0. One task leaving the same step
  twice is two events, not duplicates.
- Outside coverage and adoption: `labels_total` (agent route rows in the
  window), `owner_only_labels` (`event_id` NULL), `non_routing_labels` (non-null
  `event_id` and `inputs.routing_exit = false`, so it does not overlap
  `owner_only_labels`).
- `overridden`: agent route rows in the window with `reversed_at` set.
  `override_rate = overridden / labels_total`.
- Per class: `decided`, `predicted`, `agreed`, `coverage`, `precision` over
  route rows created in the window, from every decision source.

## Where System 1 plugs in

All three stay behind the same function, so each is a small change.

| Plug-in point | Use | Condition to start |
| --- | --- | --- |
| Routing step | Suggest the handler for a step when no rule matches | 100 shadow routing decisions and precision at or above the class target |
| Decision request pre-fill | Pre-select the predicted option on the card. The person still answers. | 100 shadow decisions for the class. Pre-selection must not change the order (recommended stays first). |
| Compact timing | Shorten or lengthen the delay before an answer takes effect, by class and predicted answer time | Median time to answer is stable for the class over 30 days |

None of these is built in this PR.

## Gating by class

| Class | Shadow | Suggest or pre-fill | Auto-apply |
| --- | --- | --- | --- |
| `reversible` | Yes | Yes, after readiness | **Yes, only here.** The 24 h veto window applies. A person can undo within 24 h. |
| Any other class (irreversible, spend, production deploy, public publication, product direction) | Yes | Pre-fill only | **Never** |

Auto-apply needs all of: class is `reversible`, class readiness met, the 24 h
veto visible to the person, and a kill switch. Reversal rate above the agreed
limit turns auto-apply off for the class.

## Vendor-swap layer

The predictor sits behind one signature:

```go
Predict(precedents []string) Prediction
```

- Today: majority vote over precedents (retrieval baseline).
- Later: a fine-tuned model or a hosted model behind the same call. Callers,
  the table and the report do not change.
- `predicted_key`, `predicted_confidence` and `predicted_precedents` are
  stored with each row. Two predictors can be compared on the same rows by
  adding one column set per predictor. Do that when a second predictor exists.

## Privacy

- The table holds facts only: ids, class names, counts, positions, keys,
  timestamps and the agent's confidence.
- It holds no ask text. Option labels are ask text and can carry secrets or
  DM content, so only the option count and the recommended position are kept.
  A test posts a label that looks like a key and checks that the whole row
  holds no label, decision or context text. The waiver contract keeps this:
  its fields are closed values and option keys, with no free-text reason.
- It holds no DM message text and no answer free text. `free_text` is a key,
  not the content.
- Routing rows hold the workflow name and status key as the class. Both are
  admin-set names, not user input.
- Training or fine-tune data is built from this table plus public ask fields.
  DM contents never enter it. Export is a separate, reviewed step.

## Jev availability (slice B0 probe, 8 Nov 2026 02:41 to 02:44 ICT)

Read-only probe for the burn-in plan (CHE-4383, slice CHE-4467). Result: Jev
is **not available**, so no `jev@<version>` predictor runs in burn-in.

| Check | Command | Exit | Result |
| --- | --- | --- | --- |
| Jev registration | `multica workspace get --output json` (read `settings`) | 0 | No `agent_decisions_jev` key: Jev delegation is off, no agent id, no mode. |
| Brief | `multica decision brief` | 0 | `enabled: true`, 0 decisions, 0 pilot runs. |
| sub2api model list | `GET /v1/models` with the `hindsight-x99` key (key not recorded) | 0 | HTTP 200, 12 models, none contains `jev`. `jev-latest` is not served for this key group. |

- Same as the 1 Nov 2026 finding (CHE-2946): sub2api does not serve `jev-latest`.
- To enable Jev later: the owner registers the agent with `PUT /api/agent-decisions/jev`, and sub2api must serve `jev-latest` for the key group. Re-run this probe then.

## Fine-tune criteria

Fine-tune only when the retrieval baseline shows a **specific gap**:

1. At least 100 decided rows in the class (the readiness gate).
2. Precision on the "other" slice is below the class target while the
   recommended slice is high. That means the baseline only follows position.
3. The gap is in a class where a better prediction would change an action
   (suggest, pre-fill or auto-apply of a `reversible` class).
4. A fresh export, checked for privacy, covers at least the 549 answers
   already on record plus the shadow rows.

If the baseline meets the target, do not fine-tune.

## Deferred

- Backfill of the 549 existing answers into the table.
- Blind audit shuffle.
- Per-project tiering of classes.
- A second predictor for side-by-side comparison.
