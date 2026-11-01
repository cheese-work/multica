package service

import (
	"context"
	"encoding/json"
	"testing"

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
