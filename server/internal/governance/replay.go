package governance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/multica-ai/multica/server/pkg/jev"
)

// ReplayScenarioFile is the top-level shape of a replay fixture file: a
// list of independent, self-contained scenarios. See
// testdata/replay_scenarios.json for the canonical public-safe fixture set,
// and cmd/govreplay for the CLI that runs it.
type ReplayScenarioFile struct {
	Scenarios []ReplayScenario `json:"scenarios"`
}

// ReplayScenario is one offline case: an already-built evaluator Input plus
// the exact scripted wire-shape answers a fake TypeSafe server should
// return, and the expected decision. Everything here is fixture data
// authored for this repository — never an unredacted historical comment.
type ReplayScenario struct {
	ID          string                     `json:"id"`
	Description string                     `json:"description"`
	Input       ReplayInput                `json:"input"`
	Answers     map[string]json.RawMessage `json:"answers"`
	Want        ReplayWant                 `json:"want"`
}

// ReplayInput is the JSON-friendly mirror of [Input]. Candidate/span option
// labels ("candidate_01", "span_01", ...) inside Answers must match the
// order these lists are declared in, per [candidateLabel] and [spanLabel].
type ReplayInput struct {
	State                         string            `json:"state"`
	Candidates                    []ReplayCandidate `json:"candidates"`
	Spans                         []ReplaySpan      `json:"spans"`
	MechanicalPreparationEligible bool              `json:"mechanical_preparation_eligible"`
	AccountableIndex              int               `json:"accountable_index"`
}

// ReplayCandidate is the JSON-friendly mirror of [Candidate].
type ReplayCandidate struct {
	ID          string `json:"id"`
	IsSquad     bool   `json:"is_squad"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// ReplaySpan is the JSON-friendly mirror of [Span].
type ReplaySpan struct {
	Text        string `json:"text"`
	StartOffset int    `json:"start_offset"`
	EndOffset   int    `json:"end_offset"`
}

// ReplayWant is the scenario's expected outcome. Set either Action (with an
// optional CandidateID to also pin which candidate should win) or
// AbstainReason — never both.
type ReplayWant struct {
	Action        string `json:"action,omitempty"`
	CandidateID   string `json:"candidate_id,omitempty"`
	AbstainReason string `json:"abstain_reason,omitempty"`
}

func (in ReplayInput) toInput() Input {
	candidates := make([]Candidate, len(in.Candidates))
	for i, c := range in.Candidates {
		candidates[i] = Candidate(c)
	}
	spans := make([]Span, len(in.Spans))
	for i, s := range in.Spans {
		spans[i] = Span(s)
	}
	return Input{
		State:                         in.State,
		Candidates:                    candidates,
		Spans:                         spans,
		MechanicalPreparationEligible: in.MechanicalPreparationEligible,
		AccountableIndex:              in.AccountableIndex,
	}
}

// matches reports whether decision satisfies this scenario's expectation,
// and a human-readable mismatch description when it does not.
func (w ReplayWant) matches(d Decision) (bool, string) {
	if w.AbstainReason != "" {
		if d.Action != nil {
			return false, fmt.Sprintf("want abstain(%s), got action %s", w.AbstainReason, d.Action.Kind)
		}
		if string(d.AbstainReason) != w.AbstainReason {
			return false, fmt.Sprintf("want abstain(%s), got abstain(%s)", w.AbstainReason, d.AbstainReason)
		}
		return true, ""
	}
	if d.Action == nil {
		return false, fmt.Sprintf("want action %s, got abstain(%s)", w.Action, d.AbstainReason)
	}
	if string(d.Action.Kind) != w.Action {
		return false, fmt.Sprintf("want action %s, got action %s", w.Action, d.Action.Kind)
	}
	if w.CandidateID != "" && d.Action.Candidate.ID != w.CandidateID {
		return false, fmt.Sprintf("want candidate %s, got %s", w.CandidateID, d.Action.Candidate.ID)
	}
	return true, ""
}

// ReplayResult is one scenario's outcome: the decision the evaluator
// actually produced, whether it matched the fixture's expectation, and any
// mismatch or evaluation-error detail for a human or CHE-697's baseline
// comparison to read.
type ReplayResult struct {
	ID              string   `json:"id"`
	Description     string   `json:"description"`
	Decision        Decision `json:"decision"`
	MatchesExpected bool     `json:"matches_expected"`
	Mismatch        string   `json:"mismatch,omitempty"`
	EvalError       string   `json:"eval_error,omitempty"`
}

// LoadReplayScenarios reads and parses a replay fixture file from path.
func LoadReplayScenarios(path string) (ReplayScenarioFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReplayScenarioFile{}, fmt.Errorf("governance: read replay fixture %s: %w", path, err)
	}
	var file ReplayScenarioFile
	if err := json.Unmarshal(data, &file); err != nil {
		return ReplayScenarioFile{}, fmt.Errorf("governance: parse replay fixture %s: %w", path, err)
	}
	return file, nil
}

// RunReplayScenario evaluates one scenario against a fresh local
// httptest.Server that serves exactly the scripted answers — proving the
// real wire path (request shape, [jev.Client] transport, strict-Choice
// decoding) end to end with no live TypeSafe API key, no network access
// beyond this local server, and no agent CLI invoked.
func RunReplayScenario(ctx context.Context, sc ReplayScenario) (ReplayResult, error) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := struct {
			Model   string                     `json:"model"`
			Answers map[string]json.RawMessage `json:"answers"`
			Usage   struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}{Model: jev.DefaultModel, Answers: sc.Answers}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	client, err := jev.NewClient(jev.Options{
		APIKey:     "govreplay-fake-key-not-a-real-credential",
		BaseURL:    srv.URL,
		RetryCount: -1,
	})
	if err != nil {
		return ReplayResult{}, fmt.Errorf("governance: build replay client: %w", err)
	}

	decision, evalErr := Evaluate(ctx, client, sc.Input.toInput())
	result := ReplayResult{ID: sc.ID, Description: sc.Description, Decision: decision}
	result.MatchesExpected, result.Mismatch = sc.Want.matches(decision)
	if evalErr != nil {
		result.EvalError = evalErr.Error()
	}
	return result, nil
}

// RunReplayScenarios runs every scenario in file in order and returns their
// results. It never returns a non-nil error for a scenario producing an
// unexpected decision — that shows up as MatchesExpected=false in the
// corresponding result — only for an infrastructure failure that prevented
// running a scenario at all.
func RunReplayScenarios(ctx context.Context, file ReplayScenarioFile) ([]ReplayResult, error) {
	results := make([]ReplayResult, 0, len(file.Scenarios))
	for _, sc := range file.Scenarios {
		result, err := RunReplayScenario(ctx, sc)
		if err != nil {
			return results, fmt.Errorf("scenario %q: %w", sc.ID, err)
		}
		results = append(results, result)
	}
	return results, nil
}
