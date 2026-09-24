-- Jev governance routing receipts (CHE-685): one row per best-effort,
-- post-commit observation attempt of the pure governance.Evaluate evaluator
-- (server/internal/governance) against a created or edited comment.
--
-- This is D03 — receipt capture only. Nothing reads this table to act: no
-- mention, correction, or status change is ever driven by a stored receipt
-- (see server/internal/governance/receipt doc comment). Live Jev provider
-- wiring and any consumption of these receipts are later, still-blocked
-- deliveries (CHE-697 found insufficient provenance-complete historical data
-- to prove viability; this table exists to start accumulating that evidence
-- safely, off by default, with only a fake/test provider in this delivery).
--
-- No foreign keys per repo convention (see AGENTS.md) — workspace_id,
-- issue_id, and comment_id are validated in application code by the caller,
-- which already holds a loaded db.Issue / db.Comment when it observes.
--
-- Deliberately its own table, written through its own best-effort call
-- AFTER the comment transaction (and after triggerTasksForComment) commits —
-- never inside the comment INSERT/UPDATE transaction and never inside the
-- native agent-queue trigger transaction. A receipt-storage failure must
-- never fail, retry, or change the comment/routing HTTP response; see
-- CreateComment / UpdateComment call sites in server/internal/handler/comment.go.
--
-- status distinguishes three outcomes, matching
-- governance/receipt.ObservationStatus:
--   'decided' — the evaluator ran to completion (Evaluate returned a
--               Decision, action or abstention alike).
--   'shed'    — the observation was never attempted or never completed,
--               because the pool-cap-1 gate was busy or the 50ms budget
--               elapsed; shed_reason names which.
--   'error'   — the evaluator/provider call returned an error other than a
--               shed condition (e.g. a provider error abstention is still
--               'decided' with abstain_reason='provider_error'; 'error' here
--               is reserved for failures the evaluator itself could not turn
--               into a Decision, and for receipt-write-adjacent faults
--               recorded defensively).
--
-- No empty/missing outcome is ever ambiguous with "no receipt captured": a
-- row's mere existence proves an attempt was made, and status/shed_reason
-- always says why it did or did not reach a Decision. A comment with no row
-- at all (flag off, or the goroutine never got admitted) is distinguishable
-- from a comment with a 'shed' row purely by row presence — callers must
-- never treat "no row" as "governance said no action was needed".
--
-- answers is the JSON-encoded []governance.RecordedAnswer — the raw
-- per-question distributions and thresholds, kept exactly as answered per
-- the evaluator's own audit contract (see governance.RecordedAnswer doc).
-- Empty JSON array, not NULL, when the evaluator never reached an answer
-- (e.g. sheds before calling the provider, or abstains before parsing any
-- answer such as too_many_candidates).
CREATE TABLE IF NOT EXISTS governance_receipt (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id   UUID NOT NULL,
    issue_id       UUID NOT NULL,
    comment_id     UUID NOT NULL,
    trigger        TEXT NOT NULL CHECK (trigger IN ('create', 'edit')),
    status         TEXT NOT NULL CHECK (status IN ('decided', 'shed', 'error')),
    shed_reason    TEXT CHECK (shed_reason IN ('pool_busy', 'budget_exceeded', 'error')),
    abstain_reason TEXT NOT NULL DEFAULT '',
    action_kind    TEXT,
    answers        JSONB NOT NULL DEFAULT '[]'::jsonb,
    observed_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
