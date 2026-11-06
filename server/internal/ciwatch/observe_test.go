package ciwatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/system1"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeObs struct {
	got []db.CreateSystem1CIObservationParams
}

func (f *fakeObs) CreateSystem1CIObservation(_ context.Context, a db.CreateSystem1CIObservationParams) (int64, error) {
	f.got = append(f.got, a)
	return 1, nil
}

func obsRun() Run {
	return Run{WorkspaceID: uuid.NewString(), RepoOwner: "cheese-work", RepoName: "multica", RunID: 42164833838, RunAttempt: 1,
		WorkflowPath: ".github/workflows/build-cache.yml", WorkflowName: "Build cache maintenance", Event: "schedule", HeadBranch: "main", Conclusion: "failure"}
}

func TestObserveFailureWritesRuleRowWithoutLogText(t *testing.T) {
	const planted = "PLANTED-LOG-LINE-d41d8cd9"
	run := obsRun()
	ev := &FailureEvent{Conclusion: "failure", Evidence: EvidenceFull, SignatureHash: "abc123", FailedJobCount: 1,
		Jobs: []JobEvidence{{JobID: 7, Name: "build", Excerpt: "API rate limit exceeded " + planted}}}
	w := &fakeObs{}
	rem := 4200
	o := Observer{W: w}
	err := o.Observe(context.Background(), run, Result{Kind: KindEvidence, Event: ev}, History{FlakeCount7d: 1, Extra: Extra{RateLimitRemaining: &rem}})
	if err != nil || len(w.got) != 1 {
		t.Fatalf("err=%v rows=%d", err, len(w.got))
	}
	p := w.got[0]
	if p.Class != "infra_rate_limit" || p.ChoiceKey != "P1" {
		t.Errorf("row = class %q choice %q", p.Class, p.ChoiceKey)
	}
	if strings.Contains(string(p.Inputs), planted) || strings.Contains(string(p.Inputs), "API rate limit") {
		t.Errorf("inputs contain log text: %s", p.Inputs)
	}
	var in map[string]any
	if err := json.Unmarshal(p.Inputs, &in); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"workflow_path": run.WorkflowPath, "event": "schedule", "branch": "main", "conclusion": "failure",
		"signature_hash": "abc123", "evidence": "full", "flake_counter": float64(1), "rate_limit_remaining": float64(4200), "shadow_only": false} {
		if in[k] != want {
			t.Errorf("inputs[%s] = %v, want %v", k, in[k], want)
		}
	}
	if jobs, _ := in["failed_jobs"].([]any); len(jobs) != 1 || jobs[0] != "build" {
		t.Errorf("failed_jobs = %v", in["failed_jobs"])
	}
	// Redelivery writes the same subject id, so a unique index (or the caller's Store gate) can dedupe it.
	_ = o.Observe(context.Background(), run, Result{Kind: KindEvidence, Event: ev}, History{})
	if w.got[1].SubjectID != p.SubjectID {
		t.Errorf("subject id must be stable per (repo, run, attempt)")
	}
}

func TestObserveUnknownIsShadowOnly(t *testing.T) {
	w := &fakeObs{}
	ev := &FailureEvent{Conclusion: "failure", Evidence: EvidencePartial, PartialReason: PartialBudget, FailedJobCount: 1}
	if err := (Observer{W: w}).Observe(context.Background(), obsRun(), Result{Kind: KindEvidence, Event: ev}, History{}); err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	json.Unmarshal(w.got[0].Inputs, &in)
	if w.got[0].Class != "unknown" || w.got[0].ChoiceKey != "P2" || in["shadow_only"] != true || in["route"] != "triage" || in["partial_reason"] != "budget" {
		t.Errorf("row = %+v inputs=%v", w.got[0], in)
	}
}

func TestObserveP0Rows(t *testing.T) {
	for _, reason := range []string{ReasonOwnedElsewhere, ReasonCancelled, ReasonSuperseded} {
		w := &fakeObs{}
		res := Result{Kind: KindP0, Reason: reason}
		if err := (Observer{W: w}).Observe(context.Background(), obsRun(), res, History{}); err != nil || len(w.got) != 1 {
			t.Fatalf("%s: err=%v rows=%d", reason, err, len(w.got))
		}
		p := w.got[0]
		if p.Class != reason || p.ChoiceKey != "P0" {
			t.Errorf("%s: row = %+v", reason, p)
		}
	}
}

func TestObserveSkipsDroppedAndLabelOnly(t *testing.T) {
	for _, k := range []Kind{KindDropped, KindLabelOnly} {
		w := &fakeObs{}
		if err := (Observer{W: w}).Observe(context.Background(), obsRun(), Result{Kind: k}, History{}); err != nil || len(w.got) != 0 {
			t.Errorf("%s: err=%v rows=%d, want none", k, err, len(w.got))
		}
	}
}

func TestJevShadowOnlyWhenEnabledAndNeverActs(t *testing.T) {
	ev := &FailureEvent{Conclusion: "failure", Evidence: EvidenceFull, FailedJobCount: 1, Jobs: []JobEvidence{{Name: "t", Excerpt: "--- FAIL: TestX"}}}
	calls := 0
	jev := func(context.Context, map[string]any) (system1.Prediction, error) {
		calls++
		return system1.Prediction{Key: "P3", Confidence: 0.7, Precedents: 12}, nil
	}
	w := &fakeObs{}
	// Disabled: the predictor is not called, no prediction columns.
	if err := (Observer{W: w, Jev: jev}).Observe(context.Background(), obsRun(), Result{Kind: KindEvidence, Event: ev}, History{}); err != nil || calls != 0 || w.got[0].PredictedKey.Valid {
		t.Fatalf("disabled: err=%v calls=%d predicted=%+v", err, calls, w.got[0].PredictedKey)
	}
	// Enabled: prediction stored beside the rule's choice, which stays the rule's.
	w = &fakeObs{}
	if err := (Observer{W: w, Jev: jev, JevEnabled: true}).Observe(context.Background(), obsRun(), Result{Kind: KindEvidence, Event: ev}, History{}); err != nil || calls != 1 {
		t.Fatalf("enabled: err=%v calls=%d", err, calls)
	}
	p := w.got[0]
	if p.PredictedKey.String != "P3" || !p.PredictedConfidence.Valid || p.ChoiceKey != "P2" || p.Class != "code_or_test" {
		t.Errorf("row = %+v", p)
	}
	// A failing Jev never fails or changes the rule row.
	w = &fakeObs{}
	bad := func(context.Context, map[string]any) (system1.Prediction, error) {
		return system1.Prediction{}, errors.New("down")
	}
	if err := (Observer{W: w, Jev: bad, JevEnabled: true}).Observe(context.Background(), obsRun(), Result{Kind: KindEvidence, Event: ev}, History{}); err != nil || len(w.got) != 1 || w.got[0].PredictedKey.Valid {
		t.Errorf("jev error: err=%v rows=%d", err, len(w.got))
	}
}

func TestHandleCarriesRunnerLostToClassifier(t *testing.T) {
	lost := failedJob(1, "build", t0)
	lost.RunnerLost = true
	gh := &fakeGH{jobsByPage: [][]Job{{lost}}, logs: map[int64]func(context.Context) io.Reader{1: text("")}}
	c, _ := newCollector(gh, &fakeBudget{}, fakeRuns{})
	got, err := c.Handle(context.Background(), baseInput())
	if err != nil || got.Event == nil || !got.Event.RunnerLost {
		t.Fatalf("err=%v event=%+v", err, got.Event)
	}
	if d := Classify(got.Event, History{}); d.Class != ClassTransient || d.Rule != "runner_lost_metadata" {
		t.Errorf("decision = %+v", d)
	}
}
