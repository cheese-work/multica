package handler

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// The web reads the DM's chosen preset from the session response: null =
// inherit, `base` or a name = explicit.
func TestChatSessionResponseCarriesPresetName(t *testing.T) {
	if got := chatSessionToResponse(db.ChatSession{}).PresetName; got != nil {
		t.Fatalf("preset_name = %q, want null for inherit", *got)
	}
	got := chatSessionToResponse(db.ChatSession{PresetName: text("fast")}).PresetName
	if got == nil || *got != "fast" {
		t.Fatalf("preset_name = %v, want fast", got)
	}
}

// Run detail shows what the claim recorded and what the daemon reported; an
// unrecorded run carries no preset_run and an unreported run has applied = null.
func TestTaskResponsePresetRun(t *testing.T) {
	if got := taskToResponse(db.AgentTaskQueue{}, "").PresetRun; got != nil {
		t.Fatalf("preset_run = %+v, want nil when the claim recorded nothing", got)
	}

	row := db.AgentTaskQueue{
		PresetName:           text("fast"),
		PresetSource:         text("dm"),
		PresetSkips:          []byte(`[{"level":"issue_label","name":"old","reason":"lab_lock"}]`),
		ClaimedModel:         text("gpt-6.1-sol"),
		ClaimedThinkingLevel: text("high"),
		ClaimedServiceTier:   text(""),
		ClaimedContextArgs:   []byte(`["--autocompact","200k"]`),
	}
	run := taskToResponse(row, "").PresetRun
	if run == nil || run.Name != "fast" || run.Source != "dm" {
		t.Fatalf("preset_run = %+v", run)
	}
	if run.Applied != nil {
		t.Fatalf("applied = %+v, want nil before the daemon reports", run.Applied)
	}
	if run.Claimed.Model == nil || *run.Claimed.Model != "gpt-6.1-sol" || len(run.Claimed.ContextArgs) != 2 {
		t.Fatalf("claimed = %+v", run.Claimed)
	}
	if run.Claimed.ServiceTier == nil || *run.Claimed.ServiceTier != "" {
		t.Fatalf("claimed service_tier = %v, want empty string", run.Claimed.ServiceTier)
	}
	if len(run.Skips) != 1 || run.Skips[0].Reason != "lab_lock" {
		t.Fatalf("skips = %+v", run.Skips)
	}

	row.AppliedReportedAt = pgtype.Timestamptz{Valid: true}
	row.AppliedModel = text("")
	row.AppliedThinkingLevel = text("medium")
	row.AppliedContextArgs = []byte(`[]`)
	run = taskToResponse(row, "").PresetRun
	if run.Applied == nil || run.Applied.Model == nil || *run.Applied.Model != "" {
		t.Fatalf("applied = %+v, want empty model kept as empty string", run.Applied)
	}
	if run.Applied.ThinkingLevel == nil || *run.Applied.ThinkingLevel != "medium" {
		t.Fatalf("applied thinking_level = %v", run.Applied.ThinkingLevel)
	}
	// claimed is never overwritten by applied
	if *run.Claimed.Model != "gpt-6.1-sol" {
		t.Fatalf("claimed model changed to %q", *run.Claimed.Model)
	}

	b, _ := json.Marshal(run)
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "source", "skips", "claimed", "applied"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("wire preset_run lacks %q: %s", key, b)
		}
	}
}

// Both interactive list queries feed the picker, so each row shape must carry
// the saved choice: named, `base` and inherit (null).
func TestChatSessionListRowCarriesPresetName(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  db.ListChatSessionsByCreatorRow
		want *string
	}{
		{"named", db.ListChatSessionsByCreatorRow{PresetName: text("fast")}, ptr("fast")},
		{"base", db.ListChatSessionsByCreatorRow{PresetName: text("base")}, ptr("base")},
		{"inherit", db.ListChatSessionsByCreatorRow{}, nil},
	} {
		got := chatSessionListRowToResponse(tc.row).PresetName
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: preset_name = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The `status=all` query returns the same shape; converting must not drop a field.
func TestChatSessionListAllRowConvertsWithPresetName(t *testing.T) {
	row := db.ListAllChatSessionsByCreatorRow{PresetName: text("fast")}
	got := chatSessionListRowToResponse(db.ListChatSessionsByCreatorRow(row)).PresetName
	if got == nil || *got != "fast" {
		t.Fatalf("preset_name = %v, want fast", got)
	}
}
