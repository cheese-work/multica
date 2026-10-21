package service

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Offline join of the shadow ledger to what actually happened (CHE-1919, CHE-1917
// Slice 0b). Read only: nothing here admits, folds, delays or skips a run, and a
// disposition is a measurement, never a delivery receipt. See docs/run-gate-ledger.md.

// Dispositions. A run state, an explicit producer refusal and a missing record
// stay distinct: no mutation seen is never read as a safe no-op.
const (
	DispRunCompletedMutated            = "run_completed_mutated"
	DispRunCompletedMutationAmbiguous  = "run_completed_mutation_ambiguous"
	DispRunCompletedNoRecordedMutation = "run_completed_no_recorded_mutation"
	DispRunFailed                      = "run_failed"
	DispRunCancelled                   = "run_cancelled"
	DispRunInFlight                    = "run_in_flight"
	DispNoRunBlocked                   = "no_run_blocked"
	DispNoRunSteered                   = "no_run_steered"
	DispNoRunNoReceipt                 = "no_run_no_receipt"
	DispReceiptConsumedNoRun           = "receipt_consumed_no_run"
	DispReceiptPending                 = "receipt_pending"
	DispReceiptDeferred                = "receipt_deferred_active_run"
	DispUnknownNoAdmissionRecord       = "unknown_no_admission_record"
	DispUnknownAdmissionValue          = "unknown_admission_value"
	DispUnknownRunMissing              = "unknown_run_missing"
	DispUnknownReceiptMissing          = "unknown_receipt_missing"
)

// Mutation kinds and how a mutation was tied to a run.
const (
	MutationCommentPosted   = "comment_posted"
	MutationStatusChanged   = "status_changed"
	MutationAssigneeChanged = "assignee_changed"
	MutationWakeupCreated   = "wakeup_created"
	MutationIssueCreated    = "issue_created"

	AttributionTaskID          = "task_id"
	AttributionWindow          = "window"
	AttributionWindowAmbiguous = "window_ambiguous"
)

// RunGateMutation is something a run did, with how it was tied to the run.
type RunGateMutation struct {
	Kind        string `json:"kind"`
	Attribution string `json:"attribution"`
	Count       int64  `json:"count"`
}

// RunGateRun is a run linked to an event by native evidence.
type RunGateRun struct {
	TaskID        string            `json:"task_id"`
	Status        string            `json:"status"`
	FailureReason string            `json:"failure_reason,omitempty"`
	Links         []string          `json:"links"`
	CreatedAt     time.Time         `json:"created_at"`
	StartedAt     *time.Time        `json:"started_at,omitempty"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
	Mutations     []RunGateMutation `json:"mutations"`
}

// RunGateReceipt is the wakeup receipt an event wrote, as it stands now.
type RunGateReceipt struct {
	TaskID    string
	Processed bool
	Deferred  bool
	Outcome   string
}

// RunGateJoin is one ledger event with its disposition. Event identity, native
// fold eligibility, queue state and active run come straight from the ledger.
type RunGateJoin struct {
	EventID            string           `json:"event_id"`
	Source             string           `json:"source"`
	EventKey           string           `json:"event_key"`
	CandidateKey       string           `json:"candidate_key"`
	IssueID            string           `json:"issue_id"`
	AgentID            string           `json:"agent_id,omitempty"`
	Origin             string           `json:"origin"`
	NonLeaderReply     bool             `json:"non_leader_agent_reply"`
	PRHeadSHA          string           `json:"pr_head_sha,omitempty"`
	EventTime          time.Time        `json:"event_time"`
	SeenCount          int32            `json:"seen_count"`
	NativeFoldEligible bool             `json:"native_fold_eligible"`
	NativeFoldTaskID   string           `json:"native_fold_task_id,omitempty"`
	ActiveTaskID       string           `json:"active_task_id,omitempty"`
	QueueStates        []string         `json:"queue_states"`
	AdmissionStatus    string           `json:"admission_status,omitempty"`
	AdmissionReason    string           `json:"admission_reason,omitempty"`
	Disposition        string           `json:"disposition"`
	Reason             string           `json:"reason,omitempty"`
	PrimaryTaskID      string           `json:"primary_task_id,omitempty"`
	Runs               []RunGateRun     `json:"runs"`
	Counterfactuals    []RunGateCfEvent `json:"counterfactuals,omitempty"`
}

var runGateInFlight = map[string]bool{"queued": true, "dispatched": true, "running": true, "waiting_local_directory": true, "deferred": true}

// ClassifyRunGateEvent joins one event to its runs and receipt. runs and receipt
// are native evidence only; receipt is nil when the event wrote none or it
// expired. It never infers a disposition from timing or from an absent mutation.
func ClassifyRunGateEvent(ev db.RunGateEvent, runs []RunGateRun, receipt *RunGateReceipt) RunGateJoin {
	j := RunGateJoin{
		EventID: util.UUIDToString(ev.ID), Source: ev.Source, EventKey: ev.EventKey, CandidateKey: ev.CandidateKey,
		IssueID: util.UUIDToString(ev.IssueID), AgentID: uuidOrEmpty(ev.AgentID), Origin: ev.Origin,
		NonLeaderReply: ev.NonLeaderAgentReply, PRHeadSHA: ev.PrHeadSha.String, EventTime: ev.EventTime.Time, SeenCount: ev.SeenCount,
		NativeFoldEligible: ev.NativeFoldEligible, NativeFoldTaskID: uuidOrEmpty(ev.NativeFoldTaskID),
		ActiveTaskID: uuidOrEmpty(ev.ActiveTaskID), QueueStates: queueStates(ev.QueueSnapshot),
		AdmissionStatus: ev.AdmissionStatus.String, AdmissionReason: ev.AdmissionReason.String,
		Runs: rankRuns(runs),
	}
	if len(j.Runs) > 0 {
		j.PrimaryTaskID, j.Disposition = j.Runs[0].TaskID, runDisposition(j.Runs[0])
		return j
	}
	if ev.Source == RunGateSourceComment {
		j.Disposition, j.Reason = classifyCommentWithoutRun(ev)
	} else {
		j.Disposition, j.Reason = classifyWakeupWithoutRun(ev, receipt)
	}
	return j
}

func classifyCommentWithoutRun(ev db.RunGateEvent) (string, string) {
	reason := ev.AdmissionReason.String
	switch ev.AdmissionStatus.String {
	case "":
		return DispUnknownNoAdmissionRecord, ""
	case "blocked":
		return DispNoRunBlocked, reason
	case "steered":
		return DispNoRunSteered, reason
	case "queued", "coalesced", "deferred":
		// The producer said a run covers it; none is joined (purged, comment
		// deleted, or not visible yet). Unknown, not a no-op.
		return DispUnknownRunMissing, ev.AdmissionStatus.String
	}
	return DispUnknownAdmissionValue, ev.AdmissionStatus.String
}

func classifyWakeupWithoutRun(ev db.RunGateEvent, r *RunGateReceipt) (string, string) {
	switch {
	case r == nil && !ev.ReceiptID.Valid:
		return DispNoRunNoReceipt, ""
	case r == nil:
		return DispUnknownReceiptMissing, ""
	case r.TaskID != "":
		return DispUnknownRunMissing, "receipt_task_missing"
	case r.Processed && r.Outcome != "":
		return DispReceiptConsumedNoRun, r.Outcome
	case r.Processed:
		return DispReceiptConsumedNoRun, "outcome_not_on_receipt"
	case r.Deferred:
		return DispReceiptDeferred, ""
	}
	return DispReceiptPending, ""
}

// runDisposition reads a run's own state. A mutation we cannot tie to the run
// (window_ambiguous) never counts as proof it acted, and no mutation seen is
// reported as exactly that.
func runDisposition(r RunGateRun) string {
	switch {
	case runGateInFlight[r.Status]:
		return DispRunInFlight
	case r.Status == "failed":
		return DispRunFailed
	case r.Status == "cancelled":
		return DispRunCancelled
	}
	ambiguous := false
	for _, m := range r.Mutations {
		if m.Count == 0 {
			continue
		}
		if m.Attribution != AttributionWindowAmbiguous {
			return DispRunCompletedMutated
		}
		ambiguous = true
	}
	if ambiguous {
		return DispRunCompletedMutationAmbiguous
	}
	return DispRunCompletedNoRecordedMutation
}

var runGateLinkRank = map[string]int{"delivered": 3, "receipt": 3, "coalesced": 2, "trigger": 1}

// rankRuns orders runs best cover first: a run that is not cancelled, then the
// strongest link (delivered or receipt, coalesced, trigger), then the earliest.
func rankRuns(runs []RunGateRun) []RunGateRun {
	out := append([]RunGateRun(nil), runs...)
	best := func(r RunGateRun) int {
		top := 0
		for _, l := range r.Links {
			top = max(top, runGateLinkRank[l])
		}
		return top
	}
	sort.SliceStable(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if (x.Status == "cancelled") != (y.Status == "cancelled") {
			return y.Status == "cancelled"
		}
		if bx, by := best(x), best(y); bx != by {
			return bx > by
		}
		if !x.CreatedAt.Equal(y.CreatedAt) {
			return x.CreatedAt.Before(y.CreatedAt)
		}
		return x.TaskID < y.TaskID
	})
	return out
}

func queueStates(snapshot []byte) []string {
	var tasks []struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(snapshot, &tasks)
	states := make([]string, 0, len(tasks))
	for _, t := range tasks {
		states = append(states, t.Status)
	}
	return states
}
