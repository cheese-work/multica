package handler

import (
	"testing"
)

// CHE-2767: a rolled-over run's final reply is written into the successor DM
// with the run's task_id. Clients must tell it from a reply to a turn asked in
// that DM, so the row names the DM the task actually ran in.
func TestMarkCarriedRepliesNamesTheTasksOwnDM(t *testing.T) {
	task := func(id string) *string { return &id }
	rows := []ChatMessageResponse{
		{ID: "carried", ChatSessionID: "new", Role: "assistant", TaskID: task("old-run")},
		{ID: "asked-here", ChatSessionID: "new", Role: "assistant", TaskID: task("new-run")},
		{ID: "user", ChatSessionID: "new", Role: "user", TaskID: task("old-run")},
		{ID: "no-task", ChatSessionID: "new", Role: "assistant"},
		{ID: "unknown-task", ChatSessionID: "new", Role: "assistant", TaskID: task("gone")},
	}
	markCarriedReplies(rows, map[string]string{"old-run": "old", "new-run": "new"})

	want := map[string]string{"carried": "old"}
	for _, row := range rows {
		got := ""
		if row.CarriedFromSessionID != nil {
			got = *row.CarriedFromSessionID
		}
		if got != want[row.ID] {
			t.Errorf("%s: carried_from_session_id = %q, want %q", row.ID, got, want[row.ID])
		}
	}
}
