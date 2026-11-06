package ciwatch

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/system1"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Extra holds the optional features only the caller can read (Multica PR data, run stats).
type Extra struct {
	RunnerLabel           string
	DurationVsMedian      *float64
	RateLimitRemaining    *int
	MinutesSinceHeadMerge *int
	ChangedPaths          []string // paths only
}

// History is what the caller knows from earlier events. Classify reads FlakeCount7d only.
type History struct {
	FlakeCount7d     int // flake counter for this signature, last 7 days
	SameSignature24h int
	SameSignature7d  int
	Last10           []string // last 10 conclusions of this workflow
	PrevAttemptGreen bool
	OwnerIssueOpen   bool // an open issue already owns this signature
	Extra
}

// Jev is the optional shadow predictor (key space: P1..P4). Nothing acts on its answer.
type Jev func(ctx context.Context, features map[string]any) (system1.Prediction, error)

// ObservationWriter is the one sqlc query Observer needs.
type ObservationWriter interface {
	CreateSystem1Observation(ctx context.Context, arg db.CreateSystem1ObservationParams) error
}

// Observer writes system1_observation rows (kind=ci_event, decision_source=rule).
// Call it only when Store reported a first insert (or for a P0 row), so a
// redelivered run adds no row.
type Observer struct {
	W          ObservationWriter
	Jev        Jev  // nil = off
	JevEnabled bool // the jev_enabled switch
	Log        *slog.Logger
}

var observationNS = uuid.MustParse("6f0a8a3e-5d0c-4c1e-9a55-3c1e0b6a4f10")

// subjectID is stable per (repo, run, attempt).
func subjectID(r Run) pgtype.UUID {
	return pgtype.UUID{Bytes: uuid.NewSHA1(observationNS, []byte(fmtKey(r))), Valid: true}
}

func fmtKey(r Run) string {
	b, _ := json.Marshal([]any{r.RepoOwner, r.RepoName, r.RunID, r.RunAttempt})
	return string(b)
}

// Observe records res. Dropped and label-only results write nothing. Features
// are metadata, job names and the signature hash; no log text ever enters the row.
func (o Observer) Observe(ctx context.Context, run Run, res Result, h History) error {
	feats := map[string]any{
		"workflow_path": run.WorkflowPath, "event": run.Event, "branch": run.HeadBranch, "conclusion": run.Conclusion,
	}
	var class, choice string
	switch res.Kind {
	case KindP0:
		class, choice = res.Reason, "P0"
	case KindEvidence:
		ev := res.Event
		d := Classify(ev, h)
		class, choice = string(d.Class), d.Protocol
		names := []string{}
		for _, j := range ev.Jobs {
			names = append(names, j.Name)
		}
		feats["failed_jobs"], feats["failed_job_count"] = names, ev.FailedJobCount
		feats["signature_hash"] = ev.SignatureHash
		feats["evidence"], feats["partial_reason"] = ev.Evidence, ev.PartialReason
		feats["rule"], feats["confidence"], feats["shadow_only"], feats["route"] = d.Rule, d.Confidence, d.ShadowOnly, d.Route
		feats["same_signature_24h"], feats["same_signature_7d"], feats["flake_counter"] = h.SameSignature24h, h.SameSignature7d, h.FlakeCount7d
		feats["last_10_conclusions"], feats["prev_attempt_green"], feats["owner_issue_open"] = h.Last10, h.PrevAttemptGreen, h.OwnerIssueOpen
		feats["runner_label"], feats["duration_vs_median"], feats["rate_limit_remaining"] = h.RunnerLabel, h.DurationVsMedian, h.RateLimitRemaining
		feats["minutes_since_head_merge"], feats["changed_paths"] = h.MinutesSinceHeadMerge, h.ChangedPaths
	default:
		return nil
	}
	inputs, err := json.Marshal(feats)
	if err != nil {
		return err
	}
	var ws pgtype.UUID
	if err := ws.Scan(run.WorkspaceID); err != nil {
		return err
	}
	p := db.CreateSystem1ObservationParams{
		WorkspaceID: ws, Kind: "ci_event", SubjectID: subjectID(run), Class: class,
		DecisionSource: pgtype.Text{String: "rule", Valid: true}, Inputs: inputs,
		ChoiceKey: pgtype.Text{String: choice, Valid: true},
	}
	if o.JevEnabled && o.Jev != nil && res.Kind == KindEvidence {
		if pred, err := o.Jev(ctx, feats); err != nil {
			if o.Log != nil {
				o.Log.Warn("ci jev shadow call failed", "run_id", run.RunID, "error", err)
			}
		} else if pred.Key != "" {
			p.PredictedKey = pgtype.Text{String: pred.Key, Valid: true}
			p.PredictedConfidence = pgtype.Float4{Float32: float32(pred.Confidence), Valid: true}
			p.PredictedPrecedents = pgtype.Int4{Int32: int32(pred.Precedents), Valid: true}
		}
	}
	return o.W.CreateSystem1Observation(ctx, p)
}
