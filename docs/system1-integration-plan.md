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
| `recommended_index` | Not used | Position of the recommended option as shown. `0` after the first-position rule. Empty if the recommendation names no option. |
| `agent_confidence` | Not used | The agent's own confidence, 0 to 1. Optional ask field `confidence`. |
| `predicted_key`, `predicted_confidence`, `predicted_precedents`, `predicted_at` | Shadow prediction | Shadow prediction, stored in the same transaction as the request |
| `choice_key`, `chosen_index`, `answered_at`, `time_to_answer_ms` | Chosen handler (`agent:<id>` or `member:<id>`) | The human answer. `choice_key` is `recommended`, `other`, `option` or `free_text`. |
| `reversed_at` | A person reassigned the issue within 1 hour | The answer was disclaimed |

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

## Recommended option stays first

- The request API (`RequireRecommendedFirst`) moves the recommended option to
  index 0 and refuses an ask it cannot resolve. Two ways to name it:
  - **Structured (preferred):** optional ask field `recommended_option`, the
    exact label (case-insensitive). It wins over the text. A value that
    matches no label is refused (400).
  - **Text:** the `recommendation` field. A label that starts the text wins,
    after markdown and a filler word such as "Option" or "I recommend" are
    skipped; the longest label wins. Otherwise exactly one label must appear
    as whole words ("I recommend Hold because CI is red"). A contrast
    ("Hold rather than Proceed", "instead of", "over", "vs") falls back to the
    words before it. A label with a negation next to it ("not Proceed") or
    inside a longer label ("Hold" in "Hold off") does not count.
- An ask with two or more options and no resolvable recommended option is
  refused with a 400 that says to set `recommended_option`. So no stored ask
  breaks the first-position rule, and `recommended_index` is always `0`.
  Asks with fewer than two options (actions) are not affected. Asks the
  server creates itself (for example run-budget decisions) do not pass this
  gate; the observation records an empty `recommended_index` for them.
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

- Per kind and class: decided, predicted, agreed, the same three for the
  "other" slice, reversed, median time to answer.
- `ready` is true once a kind has 100 decided rows. Report precision and
  coverage by class at that point.
- Precision = agreed / predicted. Coverage = predicted / decided.

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
  holds no label, decision or context text.
- It holds no DM message text and no answer free text. `free_text` is a key,
  not the content.
- Routing rows hold the workflow name and status key as the class. Both are
  admin-set names, not user input.
- Training or fine-tune data is built from this table plus public ask fields.
  DM contents never enter it. Export is a separate, reviewed step.

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
