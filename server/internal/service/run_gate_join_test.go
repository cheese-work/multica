package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1919 (CHE-1917 Slice 0b): the classifier turns native join evidence into a
// disposition. A missing run, an expired receipt and a run that changed nothing
// we can see stay distinct; none of them is read as a safe no-op.

func gateText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func gateEvent(source, admission, reason string, receipt bool) db.RunGateEvent {
	ev := db.RunGateEvent{
		ID: util.MustParseUUID("00000000-0000-0000-0000-000000000001"), Source: source, EventKey: "k", CandidateKey: "c",
		AdmissionStatus: gateText(admission), AdmissionReason: gateText(reason), SeenCount: 1,
	}
	if receipt {
		ev.ReceiptID = util.MustParseUUID("00000000-0000-0000-0000-0000000000a1")
	}
	return ev
}

func gateRun(id, status string, links []string, muts ...RunGateMutation) RunGateRun {
	return RunGateRun{TaskID: id, Status: status, Links: links, CreatedAt: time.Unix(100, 0), Mutations: muts}
}

func TestClassifyRunGateEvent(t *testing.T) {
	native := RunGateMutation{Kind: MutationCommentPosted, Attribution: AttributionTaskID, Count: 1}
	ambiguous := RunGateMutation{Kind: MutationStatusChanged, Attribution: AttributionWindowAmbiguous, Count: 1}
	windowed := RunGateMutation{Kind: MutationStatusChanged, Attribution: AttributionWindow, Count: 1}
	cases := []struct {
		name        string
		ev          db.RunGateEvent
		runs        []RunGateRun
		receipt     *RunGateReceipt
		disposition string
		reason      string
		primary     string
	}{
		{"completed with a native mutation", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "completed", []string{"trigger"}, native)}, nil, DispRunCompletedMutated, "", "t1"},
		{"completed with only a windowed status change", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "completed", []string{"trigger"}, windowed)}, nil, DispRunCompletedMutated, "", "t1"},
		{"completed with only an ambiguous status change", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "completed", []string{"trigger"}, ambiguous)}, nil, DispRunCompletedMutationAmbiguous, "", "t1"},
		{"completed with nothing recorded", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "completed", []string{"trigger"})}, nil, DispRunCompletedNoRecordedMutation, "", "t1"},
		{"failed keeps failed even with a mutation", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "failed", []string{"trigger"}, native)}, nil, DispRunFailed, "", "t1"},
		{"only a cancelled run", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "cancelled", []string{"trigger"})}, nil, DispRunCancelled, "", "t1"},
		{"a live run covers a cancelled duplicate", gateEvent("comment_leader", "coalesced", "coalesced", false),
			[]RunGateRun{gateRun("t1", "cancelled", []string{"trigger"}), gateRun("t2", "completed", []string{"coalesced"})}, nil,
			DispRunCompletedNoRecordedMutation, "", "t2"},
		{"delivered beats trigger", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "completed", []string{"trigger"}), gateRun("t2", "completed", []string{"delivered"}, native)}, nil,
			DispRunCompletedMutated, "", "t2"},
		{"running", gateEvent("comment_leader", "queued", "queued", false),
			[]RunGateRun{gateRun("t1", "running", []string{"trigger"})}, nil, DispRunInFlight, "", "t1"},
		{"producer blocked, no run", gateEvent("comment_leader", "blocked", "runtime_offline", false), nil, nil,
			DispNoRunBlocked, "runtime_offline", ""},
		{"producer steered, no run", gateEvent("comment_leader", "steered", "steered", false), nil, nil, DispNoRunSteered, "steered", ""},
		{"queued but no run found", gateEvent("comment_leader", "queued", "queued", false), nil, nil, DispUnknownRunMissing, "queued", ""},
		{"no admission and no run", gateEvent("comment_leader", "", "", false), nil, nil, DispUnknownNoAdmissionRecord, "", ""},
		{"unrecognised admission", gateEvent("comment_leader", "mystery", "x", false), nil, nil, DispUnknownAdmissionValue, "mystery", ""},
		{"wakeup with no receipt written", gateEvent("wakeup_pr_merged", "", "", false), nil, nil, DispNoRunNoReceipt, "", ""},
		{"wakeup receipt missing", gateEvent("wakeup_pr_merged", "", "", true), nil, nil, DispUnknownReceiptMissing, "", ""},
		{"receipt consumed as paused", gateEvent("wakeup_pr_merged", "", "", true), nil,
			&RunGateReceipt{Processed: true, Outcome: "paused"}, DispReceiptConsumedNoRun, "paused", ""},
		{"receipt consumed, outcome not on receipt", gateEvent("wakeup_pr_merged", "", "", true), nil,
			&RunGateReceipt{Processed: true}, DispReceiptConsumedNoRun, "outcome_not_on_receipt", ""},
		{"receipt pending", gateEvent("wakeup_pr_merged", "", "", true), nil, &RunGateReceipt{}, DispReceiptPending, "", ""},
		{"receipt held for an active run", gateEvent("wakeup_pr_merged", "", "", true), nil, &RunGateReceipt{Deferred: true}, DispReceiptDeferred, "", ""},
		{"receipt run completed", gateEvent("wakeup_pr_checks_failed", "", "", true),
			[]RunGateRun{gateRun("t9", "completed", []string{"receipt"})}, &RunGateReceipt{TaskID: "t9", Processed: true},
			DispRunCompletedNoRecordedMutation, "", "t9"},
		{"receipt names a run that is gone", gateEvent("wakeup_pr_checks_failed", "", "", true), nil,
			&RunGateReceipt{TaskID: "t9", Processed: true}, DispUnknownRunMissing, "receipt_task_missing", ""},
		{"receipt reserved by a queued run", gateEvent("wakeup_pr_merged", "", "", true),
			[]RunGateRun{gateRun("t9", "queued", []string{"receipt"})}, &RunGateReceipt{TaskID: "t9"}, DispRunInFlight, "", "t9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyRunGateEvent(c.ev, c.runs, c.receipt)
			if got.Disposition != c.disposition || got.Reason != c.reason || got.PrimaryTaskID != c.primary {
				t.Fatalf("got %q/%q/%q, want %q/%q/%q", got.Disposition, got.Reason, got.PrimaryTaskID, c.disposition, c.reason, c.primary)
			}
		})
	}
}
