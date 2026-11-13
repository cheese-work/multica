package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// A background producer has no credential: auth_source is a JSON null, not a
// missing key or an empty string.
func TestRecordStatusChange_BackgroundAuthSourceIsNull(t *testing.T) {
	f, _, issueID, _ := conditionFixture(t)
	ctx := context.Background()
	issue, err := f.q.GetIssue(ctx, issueID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := RecordStatusChange(ctx, f.q, issue, "todo", "in_progress",
		StatusTrigger{Cause: StatusCause{Kind: "system", By: StatusCauseActor{Type: "system"}}, Channel: "unknown"},
		map[string]string{"workflow": "w"})
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]json.RawMessage
	if err := json.Unmarshal(row.Details, &details); err != nil {
		t.Fatal(err)
	}
	var trigger map[string]json.RawMessage
	if err := json.Unmarshal(details["trigger"], &trigger); err != nil {
		t.Fatalf("details.trigger missing: %s", row.Details)
	}
	if got := string(trigger["auth_source"]); got != "null" {
		t.Fatalf("auth_source = %s, want null", got)
	}
	if got := string(trigger["v"]); got != "1" {
		t.Fatalf("trigger v = %s, want 1", got)
	}
	if got := string(details["handoff_workflow"]); got != `"w"` {
		t.Fatalf("handoff_workflow = %s", got)
	}
	if util.UUIDToString(row.IssueID) != util.UUIDToString(issueID) || row.Action != "status_changed" {
		t.Fatalf("row = %+v", row)
	}
}

// triggerOf records a status change carrying handoffCommentID and returns the
// stored details.trigger.
func triggerOf(t *testing.T, f principalFixture, issueID pgtype.UUID, handoffCommentID string) map[string]any {
	t.Helper()
	ctx := context.Background()
	issue, err := f.q.GetIssue(ctx, issueID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := RecordStatusChange(ctx, f.q, issue, "todo", "in_progress",
		StatusTrigger{Cause: StatusCause{Kind: StatusCauseUIOrAPI, By: StatusCauseActor{Type: "system"}}, Channel: "unknown", HandoffCommentID: handoffCommentID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var details struct {
		Trigger map[string]any `json:"trigger"`
	}
	if err := json.Unmarshal(row.Details, &details); err != nil {
		t.Fatal(err)
	}
	return details.Trigger
}

func TestRecordStatusChange_HandoffCommentLink(t *testing.T) {
	f, _, issueID, _ := conditionFixture(t)
	issue := util.UUIDToString(issueID)
	own := f.Comment(t, issue, "handoff")
	f.Cleanup(t, "DELETE FROM comment WHERE issue_id=$1", issueID)

	otherIssue := f.Issue(t, "other issue")
	onOther := f.Comment(t, otherIssue, "elsewhere")

	otherWS := f.Workspace(t, "other ws", fmt.Sprintf("handoff-ws-%d", principalSeq.Add(1)))
	foreignIssue := f.Issue(t, "foreign", testutil.Cols{"workspace_id": otherWS})
	foreign := f.Comment(t, foreignIssue, "foreign", testutil.Cols{"workspace_id": otherWS})

	cases := []struct {
		name, id string
		linked   bool
	}{
		{"same issue", own, true},
		{"other issue", onOther, false},
		{"other workspace", foreign, false},
		{"unknown comment", "00000000-0000-7000-8000-000000000000", false},
		{"not a uuid", "nope", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trigger := triggerOf(t, f, issueID, c.id)
			got, linked := trigger["handoff_comment_id"]
			_, invalid := trigger["ref_invalid"]
			if c.linked && (!linked || got != c.id || invalid) {
				t.Fatalf("valid link: trigger = %v", trigger)
			}
			if !c.linked && (linked || trigger["ref_invalid"] != true) {
				t.Fatalf("invalid link must be dropped with ref_invalid: trigger = %v", trigger)
			}
		})
	}

	t.Run("no link", func(t *testing.T) {
		trigger := triggerOf(t, f, issueID, "")
		if _, ok := trigger["handoff_comment_id"]; ok || trigger["ref_invalid"] != nil {
			t.Fatalf("trigger = %v", trigger)
		}
	})
}
