package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunGateCfEvent is what one counterfactual window would have done with an event.
type RunGateCfEvent struct {
	WindowSeconds  int64  `json:"window_seconds"`
	Role           string `json:"role"`
	OpenerEventKey string `json:"opener_event_key,omitempty"`
	OpenerActiveID string `json:"opener_active_task_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// RunGateWindowSim is the outcome of one offline coalescing window over one source.
type RunGateWindowSim struct {
	Source        string         `json:"source"`
	WindowSeconds int64          `json:"window_seconds"`
	Events        int            `json:"events"`
	Opens         int            `json:"opens"`
	Folded        int            `json:"folded"`
	Protected     map[string]int `json:"protected"`
	// Of the folded events: how many the native fold could already take at
	// capture, how many the producer itself reported coalesced, how many had a
	// run that changed something (folding them would have merged away an input
	// whose own run acted), and how many have no joined outcome at all.
	FoldedNativeEligible     int `json:"folded_native_fold_eligible"`
	FoldedAdmissionCoalesced int `json:"folded_admission_coalesced"`
	FoldedWithMutation       int `json:"folded_with_mutation"`
	FoldedUnknown            int `json:"folded_unknown"`
}

// RunGateProducerEvidence is the producer-side denominator a source is measured
// against. It is a floor: an input the producer dropped before leaving any row
// is invisible to it.
type RunGateProducerEvidence struct {
	Evidenced int64 `json:"evidenced"`
	Captured  int64 `json:"captured"`
	Missing   int64 `json:"missing"`
	// RetentionLimited: receipts older than 7 days are deleted, so Evidenced
	// undercounts for a window that reaches back past that.
	RetentionLimited bool `json:"retention_limited"`
	// LedgerWithoutReceipt: ledger events whose producer wrote no receipt.
	LedgerWithoutReceipt int64 `json:"ledger_without_receipt,omitempty"`
}

// RunGateSourceReport is one source's capture completeness, volume and outcomes.
type RunGateSourceReport struct {
	Source             string                  `json:"source"`
	Captured           int                     `json:"captured"`
	Observations       int64                   `json:"observations"`
	NativeFoldEligible int                     `json:"native_fold_eligible"`
	Admission          map[string]int          `json:"admission"`
	Dispositions       map[string]int          `json:"dispositions"`
	Producer           RunGateProducerEvidence `json:"producer"`
	Windows            []RunGateWindowSim      `json:"windows"`
}

// RunGateReport is the offline Slice 0b report. Everything in it is a
// measurement; none of it is a delivery receipt or permission to suppress work.
type RunGateReport struct {
	WorkspaceID        string                `json:"workspace_id"`
	Since              time.Time             `json:"since"`
	Until              time.Time             `json:"until"`
	GeneratedAt        time.Time             `json:"generated_at"`
	LedgerFirstEventAt *time.Time            `json:"ledger_first_event_at,omitempty"`
	Sources            []RunGateSourceReport `json:"sources"`
	Events             []RunGateJoin         `json:"events"`
}

// runGateReceiptRetention is how long a processed wakeup receipt survives.
const runGateReceiptRetention = 7 * 24 * time.Hour

// RunGateWindows are the counterfactual coalescing windows.
var RunGateWindows = []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// BuildRunGateReport joins the ledger rows of [since, until) to their runs and
// receipts and measures capture completeness and the counterfactual windows.
// now only decides whether the window reaches past receipt retention.
func BuildRunGateReport(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, since, until, now time.Time) (RunGateReport, error) {
	from, to := pgtype.Timestamptz{Time: since, Valid: true}, pgtype.Timestamptz{Time: until, Valid: true}
	report := RunGateReport{WorkspaceID: util.UUIDToString(workspaceID), Since: since, Until: until, GeneratedAt: now}
	events, err := q.ListRunGateReportEvents(ctx, db.ListRunGateReportEventsParams{WorkspaceID: workspaceID, Since: from, Until: to})
	if err != nil {
		return report, err
	}
	runRows, err := q.ListRunGateRuns(ctx, db.ListRunGateRunsParams{WorkspaceID: workspaceID, Since: from, Until: to})
	if err != nil {
		return report, err
	}
	receiptRows, err := q.ListRunGateReceipts(ctx, db.ListRunGateReceiptsParams{WorkspaceID: workspaceID, Since: from, Until: to})
	if err != nil {
		return report, err
	}
	runs, err := loadRunGateRuns(ctx, q, runRows)
	if err != nil {
		return report, err
	}
	receipts := map[pgtype.UUID]*RunGateReceipt{}
	for _, r := range receiptRows {
		receipts[r.EventID] = &RunGateReceipt{TaskID: uuidOrEmpty(r.TaskID), Processed: r.ProcessedAt.Valid, Deferred: r.DeferredAt.Valid, Outcome: r.Outcome}
	}
	report.Events = make([]RunGateJoin, 0, len(events))
	for _, ev := range events {
		report.Events = append(report.Events, ClassifyRunGateEvent(ev, runs[ev.ID], receipts[ev.ID]))
	}
	if first, err := q.FirstRunGateEventTime(ctx, workspaceID); err != nil {
		return report, err
	} else if first.Valid {
		report.LedgerFirstEventAt = &first.Time
	}
	for _, source := range []string{RunGateSourceComment, RunGateSourcePRMerged, RunGateSourcePRChecksFailed} {
		sr, err := sourceReport(ctx, q, workspaceID, source, from, to, since, now, events, report.Events)
		if err != nil {
			return report, err
		}
		report.Sources = append(report.Sources, sr)
	}
	return report, nil
}

// loadRunGateRuns groups the linked runs by event and attaches what each did.
func loadRunGateRuns(ctx context.Context, q *db.Queries, rows []db.ListRunGateRunsRow) (map[pgtype.UUID][]RunGateRun, error) {
	ids := make([]pgtype.UUID, 0, len(rows))
	seen := map[pgtype.UUID]bool{}
	for _, r := range rows {
		if !seen[r.TaskID] {
			seen[r.TaskID] = true
			ids = append(ids, r.TaskID)
		}
	}
	mutations := map[pgtype.UUID][]RunGateMutation{}
	if len(ids) > 0 {
		muts, err := q.ListRunGateRunMutations(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, m := range muts {
			mutations[m.TaskID] = append(mutations[m.TaskID], RunGateMutation{Kind: m.Kind, Attribution: m.Attribution, Count: m.N})
		}
	}
	out := map[pgtype.UUID][]RunGateRun{}
	for _, r := range rows {
		run := RunGateRun{TaskID: util.UUIDToString(r.TaskID), Status: r.Status, FailureReason: r.FailureReason, CreatedAt: r.CreatedAt.Time,
			StartedAt: timePtr(r.StartedAt), CompletedAt: timePtr(r.CompletedAt), Mutations: append([]RunGateMutation{}, mutations[r.TaskID]...)}
		for link, on := range map[string]bool{"trigger": r.LinkTrigger, "coalesced": r.LinkCoalesced, "delivered": r.LinkDelivered, "receipt": r.LinkReceipt} {
			if on {
				run.Links = append(run.Links, link)
			}
		}
		sort.Strings(run.Links)
		out[r.EventID] = append(out[r.EventID], run)
	}
	return out, nil
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// sourceReport totals one source: volume, admission and disposition counts, the
// producer-side completeness figure, and the three counterfactual windows.
func sourceReport(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, source string, from, to pgtype.Timestamptz, since, now time.Time,
	events []db.RunGateEvent, joins []RunGateJoin) (RunGateSourceReport, error) {
	sr := RunGateSourceReport{Source: source, Admission: map[string]int{}, Dispositions: map[string]int{}, Windows: []RunGateWindowSim{}}
	var mine []*RunGateJoin
	for i := range joins {
		if joins[i].Source != source {
			continue
		}
		j := &joins[i]
		mine = append(mine, j)
		sr.Captured++
		sr.Observations += int64(j.SeenCount)
		sr.Dispositions[j.Disposition]++
		sr.Admission[orNone(j.AdmissionStatus)]++
		if j.NativeFoldEligible {
			sr.NativeFoldEligible++
		}
		if source != RunGateSourceComment && !events[i].ReceiptID.Valid {
			sr.Producer.LedgerWithoutReceipt++
		}
	}
	if source == RunGateSourceComment {
		row, err := q.CountRunGateCommentEvidence(ctx, db.CountRunGateCommentEvidenceParams{WorkspaceID: workspaceID, Since: from, Until: to})
		if err != nil {
			return sr, err
		}
		sr.Producer.Evidenced, sr.Producer.Captured = row.Evidenced, row.Captured
	} else {
		rows, err := q.CountRunGateWakeupEvidence(ctx, db.CountRunGateWakeupEvidenceParams{WorkspaceID: workspaceID, Since: from, Until: to})
		if err != nil {
			return sr, err
		}
		eventType := map[string]string{RunGateSourcePRMerged: "pr.merged", RunGateSourcePRChecksFailed: "pr.checks_failed"}[source]
		for _, r := range rows {
			if r.EventType == eventType {
				sr.Producer.Evidenced, sr.Producer.Captured = r.Evidenced, r.Captured
			}
		}
		sr.Producer.RetentionLimited = since.Before(now.Add(-runGateReceiptRetention))
	}
	sr.Producer.Missing = sr.Producer.Evidenced - sr.Producer.Captured
	// The simulation annotates the joins it is given; hand it this source's own.
	group := make([]RunGateJoin, len(mine))
	for i, j := range mine {
		group[i] = *j
	}
	for _, w := range RunGateWindows {
		sim := SimulateRunGateCoalescing(group, w)
		sim.Source = source
		sr.Windows = append(sr.Windows, sim)
	}
	for i, j := range mine {
		j.Counterfactuals = group[i].Counterfactuals
	}
	return sr, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// SimulateRunGateCoalescing replays joins (one source, any order) through a
// fixed leading-edge window and annotates each join in place. The first event
// of a (source, issue, agent, head) group opens a window; later events of the
// group inside it fold; one exactly a window later opens the next. The head is
// part of the group because the native merge is head-scoped. An event the
// ledger marks as an explicit mention, a worker reply or agent-less stands alone
// as protected: it neither opens nor folds.
func SimulateRunGateCoalescing(joins []RunGateJoin, window time.Duration) RunGateWindowSim {
	sim := RunGateWindowSim{WindowSeconds: int64(window.Seconds()), Protected: map[string]int{}}
	order := make([]int, len(joins))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := joins[order[a]], joins[order[b]]
		if !x.EventTime.Equal(y.EventTime) {
			return x.EventTime.Before(y.EventTime)
		}
		return x.EventID < y.EventID
	})
	openers := map[runGateGroup]int{}
	for _, i := range order {
		j := &joins[i]
		sim.Source, sim.Events = j.Source, sim.Events+1
		cf := RunGateCfEvent{WindowSeconds: sim.WindowSeconds}
		if reason := runGateProtection(*j); reason != "" {
			sim.Protected[reason]++
			cf.Role, cf.Reason = "protected", reason
		} else if g := (runGateGroup{j.Source, j.IssueID, j.AgentID, j.PRHeadSHA}); !covered(joins, openers, g, j.EventTime, window) {
			sim.Opens++
			openers[g], cf.Role = i, "open"
		} else {
			o := joins[openers[g]]
			sim.Folded++
			sim.countFolded(*j)
			cf.Role, cf.OpenerEventKey, cf.OpenerActiveID = "fold", o.EventKey, o.ActiveTaskID
		}
		j.Counterfactuals = append(j.Counterfactuals, cf)
	}
	return sim
}

func (s *RunGateWindowSim) countFolded(j RunGateJoin) {
	if j.NativeFoldEligible {
		s.FoldedNativeEligible++
	}
	if j.AdmissionStatus == "coalesced" {
		s.FoldedAdmissionCoalesced++
	}
	switch {
	case j.Disposition == DispRunCompletedMutated:
		s.FoldedWithMutation++
	case strings.HasPrefix(j.Disposition, "unknown_"):
		s.FoldedUnknown++
	}
}

type runGateGroup struct{ source, issue, agent, head string }

func covered(joins []RunGateJoin, openers map[runGateGroup]int, g runGateGroup, at time.Time, window time.Duration) bool {
	o, ok := openers[g]
	return ok && at.Before(joins[o].EventTime.Add(window))
}

func runGateProtection(j RunGateJoin) string {
	switch {
	case j.AgentID == "":
		return "no_agent"
	case j.NonLeaderReply:
		return "worker_reply"
	case j.Origin == "mention_squad_leader":
		return "explicit_mention"
	}
	return ""
}
