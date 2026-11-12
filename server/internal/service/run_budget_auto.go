package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunBudgetAutoRule names the platform rule that approves run-budget top-ups.
// It is written on every auto decision so a person can find and revoke it.
const RunBudgetAutoRule = "auto-approver v1"

// Anomaly kinds the auto-approver never approves; they escalate to a person.
const (
	RunBudgetAnomalyLoop       = "loop"
	RunBudgetAnomalyNoop       = "noop"
	RunBudgetAnomalySpike      = "spike"
	RunBudgetAnomalyChurn      = "churn"
	RunBudgetAnomalyOpenAIPool = "openai_pool"
)

// RunBudgetAutoVerdict is the auto-approver's decision for one spent window.
// Anomaly is set when the window must escalate; otherwise Approve says whether
// the top-up is granted, and Reason says why it is not.
type RunBudgetAutoVerdict struct {
	Approve bool
	Anomaly string
	Reason  string
	Runs    int
}

// Auto-approver thresholds. They are fixed in v1: the workspace setting holds
// only the kill switch and the daily envelope.
const (
	runBudgetAutoDefaultEnvelope = 300
	runBudgetAutoLoopRuns        = 3   // same failing error in this many runs of the window
	runBudgetAutoNoopPercent     = 80  // this share of runs delivering nothing
	runBudgetAutoSpikeFactor     = 3.0 // tokens per run over this multiple of the workspace median
	runBudgetAutoChurnTopUps     = 3   // top-ups on one issue within runBudgetAutoChurnWindow
	runBudgetAutoChurnWindow     = 24 * time.Hour
	runBudgetAutoTokenHistory    = 7 * 24 * time.Hour
)

// runBudgetAutoZone is the zone of the envelope's day: ICT, UTC+7.
var runBudgetAutoZone = time.FixedZone("ICT", 7*3600)

// RunBudgetAutoConfig is the workspace setting under settings.run_budget_auto.
// Absent, the auto-approver is on with the default envelope. Enabled false is
// the kill switch: every spent window then goes to a person's card as before.
type RunBudgetAutoConfig struct {
	Enabled       bool `json:"enabled"`
	DailyEnvelope int  `json:"daily_envelope"`
}

// RunBudgetAutoConfigFromSettings reads the setting. Malformed settings read as
// the kill switch off: a spend rule never acts on a document it cannot parse.
func RunBudgetAutoConfigFromSettings(settings []byte) (RunBudgetAutoConfig, error) {
	cfg := RunBudgetAutoConfig{Enabled: true, DailyEnvelope: runBudgetAutoDefaultEnvelope}
	if len(settings) == 0 {
		return cfg, nil
	}
	var doc struct {
		Auto *RunBudgetAutoConfig `json:"run_budget_auto"`
	}
	doc.Auto = &cfg
	if err := json.Unmarshal(settings, &doc); err != nil {
		return RunBudgetAutoConfig{}, fmt.Errorf("parse workspace settings for run_budget_auto: %w", err)
	}
	if cfg.DailyEnvelope < 0 {
		cfg.DailyEnvelope = 0
	}
	return cfg, nil
}

// RunBudgetAutoDayStart is midnight ICT of now's day, where the envelope resets.
func RunBudgetAutoDayStart(now time.Time) time.Time {
	t := now.In(runBudgetAutoZone)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, runBudgetAutoZone)
}

var runBudgetAutoDigits = regexp.MustCompile(`[0-9]+`)

// runBudgetAutoErrorKey makes errors that differ only in numbers (exit codes,
// ports, line numbers, ids) the same failure. It keys a structured run error,
// never comment text.
func runBudgetAutoErrorKey(msg string) string {
	key := runBudgetAutoDigits.ReplaceAllString(strings.ToLower(strings.TrimSpace(msg)), "#")
	if len(key) > 120 {
		key = key[:120]
	}
	return key
}

// EvaluateRunBudgetAuto judges the spent window that opened at since. It only
// reads: the caller acts on the verdict. Order: kill switch, anomalies (which
// are never approved), progress, then the daily envelope.
func EvaluateRunBudgetAuto(ctx context.Context, q *db.Queries, issue db.Issue, since, now time.Time) (RunBudgetAutoVerdict, error) {
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return RunBudgetAutoVerdict{}, fmt.Errorf("run budget auto: load workspace: %w", err)
	}
	cfg, err := RunBudgetAutoConfigFromSettings(ws.Settings)
	if err != nil {
		return RunBudgetAutoVerdict{Reason: "disabled"}, nil
	}
	if !cfg.Enabled {
		return RunBudgetAutoVerdict{Reason: "disabled"}, nil
	}
	usage, err := loadRunBudgetUsage(ctx, q, issue)
	if err != nil || usage == nil {
		return RunBudgetAutoVerdict{Reason: "not_budgeted"}, err
	}
	sinceTS := pgtype.Timestamptz{Time: since, Valid: true}
	runs, err := q.ListRunBudgetWindowRuns(ctx, db.ListRunBudgetWindowRunsParams{IssueID: issue.ID, Since: sinceTS})
	if err != nil {
		return RunBudgetAutoVerdict{}, fmt.Errorf("run budget auto: list runs: %w", err)
	}
	anomaly, err := runBudgetAutoAnomaly(ctx, q, issue, ws.Settings, runs, now)
	if err != nil {
		return RunBudgetAutoVerdict{}, err
	}
	if anomaly != "" {
		return RunBudgetAutoVerdict{Anomaly: anomaly, Reason: anomaly, Runs: usage.Max}, nil
	}
	delivered := 0
	for _, r := range runs {
		// A pushed head whose checks are missing or not all finished may fail
		// the same check as the others: the loop test cannot run, so a person decides.
		if r.CiUnknown {
			return RunBudgetAutoVerdict{Reason: "ci_unknown", Runs: usage.Max}, nil
		}
		if r.Delivered {
			delivered++
		}
	}
	if delivered == 0 {
		return RunBudgetAutoVerdict{Reason: "no_progress", Runs: usage.Max}, nil
	}
	used, err := q.SumAutoApprovedRunsSince(ctx, db.SumAutoApprovedRunsSinceParams{
		WorkspaceID: issue.WorkspaceID, Since: pgtype.Timestamptz{Time: RunBudgetAutoDayStart(now), Valid: true},
	})
	if err != nil {
		return RunBudgetAutoVerdict{}, fmt.Errorf("run budget auto: sum envelope: %w", err)
	}
	if used+int64(usage.Max) > int64(cfg.DailyEnvelope) {
		return RunBudgetAutoVerdict{Reason: "envelope", Runs: usage.Max}, nil
	}
	return RunBudgetAutoVerdict{Approve: true, Runs: usage.Max}, nil
}

// runBudgetAutoAnomaly returns the first anomaly kind the window shows, or "".
func runBudgetAutoAnomaly(ctx context.Context, q *db.Queries, issue db.Issue, settings []byte, runs []db.ListRunBudgetWindowRunsRow, now time.Time) (string, error) {
	failures := map[string]int{}
	var notDelivered, tokens int64
	openAI := false
	for _, r := range runs {
		// The checks and CI workflows that failed on the heads a run pushed, and a
		// failed run's own error; each counts once per run.
		keys := append([]string(nil), r.Failures...)
		if r.Status == "failed" && strings.TrimSpace(r.Error) != "" {
			keys = append(keys, runBudgetAutoErrorKey(r.Error))
		}
		seen := map[string]bool{}
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				failures[k]++
			}
		}
		if !r.Delivered {
			notDelivered++
		}
		tokens += r.Tokens
		if provider, ok := ResolveModelProvider(r.Model); ok && strings.EqualFold(provider, "openai") {
			openAI = true
		}
	}
	for _, n := range failures {
		if n >= runBudgetAutoLoopRuns {
			return RunBudgetAnomalyLoop, nil
		}
	}
	if n := int64(len(runs)); n > 0 && notDelivered*100 >= n*runBudgetAutoNoopPercent {
		return RunBudgetAnomalyNoop, nil
	}
	if n := int64(len(runs)); n > 0 {
		p50, err := q.WorkspaceTaskTokensP50(ctx, db.WorkspaceTaskTokensP50Params{
			WorkspaceID: issue.WorkspaceID, Since: pgtype.Timestamptz{Time: now.Add(-runBudgetAutoTokenHistory), Valid: true},
		})
		if err != nil {
			return "", fmt.Errorf("run budget auto: token median: %w", err)
		}
		if p50 > 0 && float64(tokens)/float64(n) > runBudgetAutoSpikeFactor*p50 {
			return RunBudgetAnomalySpike, nil
		}
	}
	topUps, err := q.CountRunBudgetTopUpsSince(ctx, db.CountRunBudgetTopUpsSinceParams{
		IssueID: issue.ID, Since: pgtype.Timestamptz{Time: now.Add(-runBudgetAutoChurnWindow), Valid: true},
	})
	if err != nil {
		return "", fmt.Errorf("run budget auto: count top-ups: %w", err)
	}
	if topUps >= runBudgetAutoChurnTopUps {
		return RunBudgetAnomalyChurn, nil
	}
	if openAI {
		holds, err := ProviderHoldsFromSettings(settings)
		if err != nil {
			return RunBudgetAnomalyOpenAIPool, nil
		}
		if _, held := findProviderHold(holds, "openai"); held {
			return RunBudgetAnomalyOpenAIPool, nil
		}
	}
	return "", nil
}
