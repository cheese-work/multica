package handler

import (
	"context"
	"testing"
	"time"
)

// Resuming a paused autopilot must re-point every enabled schedule trigger's
// next_run_at at the future; a past value left over from the pause window made the
// UI show an overdue "next run" on an autopilot that read active (CHE-5500).
func TestUpdateAutopilot_ResumeRecomputesNextRunAt(t *testing.T) {
	agentID := createWebhookTestAgent(t, "ResumeNextRun Agent")
	apID := createWebhookTestAutopilot(t, agentID, "paused", "run_only")
	enabled := seedScheduleTriggerPublishedBy(t, apID, testUserID)
	disabled := seedScheduleTriggerPublishedBy(t, apID, testUserID)

	past := time.Now().Add(-96 * time.Hour).UTC().Truncate(time.Second)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE autopilot_trigger SET next_run_at = $2 WHERE id = ANY($1::uuid[])`,
		[]string{enabled, disabled}, past); err != nil {
		t.Fatalf("seed past next_run_at: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`UPDATE autopilot_trigger SET enabled = false WHERE id = $1`, disabled); err != nil {
		t.Fatalf("disable trigger: %v", err)
	}

	patchAutopilot(t, apID, map[string]any{"status": "active"})

	got, err := testHandler.Queries.GetAutopilotTrigger(context.Background(), parseUUID(enabled))
	if err != nil {
		t.Fatalf("load enabled trigger: %v", err)
	}
	if !got.NextRunAt.Valid || !got.NextRunAt.Time.After(time.Now()) {
		t.Errorf("enabled trigger next_run_at = %v, want a future time after resume", got.NextRunAt)
	}

	skipped, err := testHandler.Queries.GetAutopilotTrigger(context.Background(), parseUUID(disabled))
	if err != nil {
		t.Fatalf("load disabled trigger: %v", err)
	}
	if !skipped.NextRunAt.Valid || !skipped.NextRunAt.Time.Equal(past) {
		t.Errorf("disabled trigger next_run_at = %v, want untouched %v", skipped.NextRunAt, past)
	}
}

// A PATCH that leaves an already-active autopilot active must not move next_run_at.
func TestUpdateAutopilot_NonResumeKeepsNextRunAt(t *testing.T) {
	agentID := createWebhookTestAgent(t, "KeepNextRun Agent")
	apID := createWebhookTestAutopilot(t, agentID, "active", "run_only")
	trig := seedScheduleTriggerPublishedBy(t, apID, testUserID)
	marker := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE autopilot_trigger SET next_run_at = $2 WHERE id = $1`, trig, marker); err != nil {
		t.Fatalf("seed next_run_at: %v", err)
	}

	patchAutopilot(t, apID, map[string]any{"title": "Renamed", "status": "active"})

	got, err := testHandler.Queries.GetAutopilotTrigger(context.Background(), parseUUID(trig))
	if err != nil {
		t.Fatalf("load trigger: %v", err)
	}
	if !got.NextRunAt.Valid || !got.NextRunAt.Time.Equal(marker) {
		t.Errorf("next_run_at = %v, want unchanged %v", got.NextRunAt, marker)
	}
}
