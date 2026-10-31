package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// applied is the daemon-reported launch selection of one claim generation (CHE-2248).
type applied struct {
	Model, Thinking, Tier *string
	ContextArgs           []byte
	ReportedAt            *string
}

func readApplied(t *testing.T, taskID string) applied {
	t.Helper()
	var a applied
	dbfx.QueryRow(t, `
		SELECT applied_model, applied_thinking_level, applied_service_tier,
		       applied_context_args, applied_reported_at::text
		FROM agent_task_queue WHERE id = $1`, taskID).
		Scan(&a.Model, &a.Thinking, &a.Tier, &a.ContextArgs, &a.ReportedAt)
	return a
}

func (a applied) isEmpty() bool {
	return a.Model == nil && a.Thinking == nil && a.Tier == nil && a.ContextArgs == nil && a.ReportedAt == nil
}

// report is the applied-settings request body.
func report(runtimeID, dispatchedAt, model string) map[string]any {
	return map[string]any{
		"runtime_id": runtimeID, "dispatched_at": dispatchedAt,
		"applied_model": model, "applied_thinking_level": "high",
		"applied_service_tier": "", "applied_context_args": []string{"--autocompact", "200k"},
	}
}

func postApplied(taskID string, body map[string]any) *http.Request {
	return withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/applied-settings", body), "taskId", taskID)
}

// rfc3339 converts a dispatched_at::text value into the wire format.
func rfc3339(t *testing.T, pgText string) string {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04:05.999999-07", pgText)
	if err != nil {
		t.Fatalf("dispatched_at %q: %v", pgText, err)
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func claimedGeneration(t *testing.T, c AgentTaskResponse) string {
	t.Helper()
	if c.DispatchedAt == nil {
		t.Fatal("claim carries no dispatched_at")
	}
	return *c.DispatchedAt
}

// claimWithCapabilities claims the next task as a daemon advertising the given capabilities.
func claimWithCapabilities(t *testing.T, runtimeID, daemonID, capabilities string) AgentTaskResponse {
	t.Helper()
	request := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID)
	request = withURLParam(request, "runtimeId", runtimeID)
	request.Header.Set("X-Client-Capabilities", capabilities)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil {
		t.Fatal("no task was claimed")
	}
	return *response.Task
}

// claimOnce claims one fresh DM task as a daemon with applied-settings-v1.
func claimOnce(t *testing.T) (claimed AgentTaskResponse, runtimeID string) {
	t.Helper()
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")
	return claimWithCapabilities(t, runtimeID, daemonID, "applied-settings-v1"), runtimeID
}

// The claim tells the daemon it may report applied settings.
func TestAppliedSettings_ClaimAdvertisesSupport(t *testing.T) {
	requireDB(t)
	claimed, _ := claimOnce(t)
	if !claimed.AppliedSettingsSupported {
		t.Fatal("claim response lacks applied_settings_supported")
	}
}

// R5 server half: a daemon without applied-settings-v1 is not told to report.
func TestAppliedSettings_R5_NoCapabilityNoSupportFlag(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")
	if claimed := claimWithCapabilities(t, runtimeID, daemonID, "rpc-v1"); claimed.AppliedSettingsSupported {
		t.Fatal("claim advertised applied_settings_supported to a daemon without the capability")
	}
}

// R3 server half: a report is stored as sent, and the claimed columns stay as claimed.
func TestAppliedSettings_StoresReportedValues(t *testing.T) {
	requireDB(t)
	claimed, runtimeID := claimOnce(t)
	before := readSnapshot(t, claimed.ID)

	testutil.Call(t, testHandler.ReportAppliedSettings,
		postApplied(claimed.ID, report(runtimeID, claimedGeneration(t, claimed), "env-model"))).Want(http.StatusOK)

	a := readApplied(t, claimed.ID)
	if str(a.Model) != "env-model" || str(a.Thinking) != "high" || str(a.Tier) != "" || a.ReportedAt == nil {
		t.Fatalf("applied = %+v", a)
	}
	if string(a.ContextArgs) != `["--autocompact", "200k"]` {
		t.Fatalf("applied context args = %s", a.ContextArgs)
	}
	if after := readSnapshot(t, claimed.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("claimed columns changed: %+v -> %+v", before, after)
	}
}

// A1: a delayed report of an older claim on the same runtime is refused.
func TestAppliedSettings_A1_OldClaimReportIsRefused(t *testing.T) {
	requireDB(t)
	taskID, runtimeID, daemonID, _, oldGeneration := agedClaim(t, true)
	reclaimed := claimForkTask(t, runtimeID, daemonID)
	if reclaimed.ID != taskID {
		t.Fatalf("reclaimed task %s, want %s", reclaimed.ID, taskID)
	}
	before := readSnapshot(t, taskID)

	testutil.Call(t, testHandler.ReportAppliedSettings,
		postApplied(taskID, report(runtimeID, rfc3339(t, oldGeneration), "old"))).Want(http.StatusConflict)

	if a := readApplied(t, taskID); !a.isEmpty() {
		t.Fatalf("applied = %+v, want all NULL", a)
	}
	if after := readSnapshot(t, taskID); !reflect.DeepEqual(before, after) {
		t.Fatalf("claim B snapshot changed: %+v -> %+v", before, after)
	}
}

// A2: another runtime's ID, or another workspace's daemon, is a 404 and writes nothing.
func TestAppliedSettings_A2_ForeignRuntimeOrWorkspaceIs404(t *testing.T) {
	requireDB(t)
	claimed, _ := claimOnce(t)
	generation := claimedGeneration(t, claimed)

	otherRuntime := dbfx.Runtime(t, "applied foreign runtime")
	testutil.Call(t, testHandler.ReportAppliedSettings,
		postApplied(claimed.ID, report(otherRuntime, generation, "x"))).Want(http.StatusNotFound)

	foreign := withURLParam(newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+claimed.ID+"/applied-settings",
		report(claimed.RuntimeID, generation, "x"), uuid.NewString(), "other-daemon"), "taskId", claimed.ID)
	testutil.Call(t, testHandler.ReportAppliedSettings, foreign).Want(http.StatusNotFound)

	if a := readApplied(t, claimed.ID); !a.isEmpty() {
		t.Fatalf("applied = %+v, want all NULL", a)
	}
}

// A3: an identical repeat is a 200 no-op; a repeat with other values is a 409.
func TestAppliedSettings_A3_DuplicateReport(t *testing.T) {
	requireDB(t)
	claimed, runtimeID := claimOnce(t)
	generation := claimedGeneration(t, claimed)
	testutil.Call(t, testHandler.ReportAppliedSettings, postApplied(claimed.ID, report(runtimeID, generation, "m1"))).Want(http.StatusOK)
	first := readApplied(t, claimed.ID)

	testutil.Call(t, testHandler.ReportAppliedSettings, postApplied(claimed.ID, report(runtimeID, generation, "m1"))).Want(http.StatusOK)
	if got := readApplied(t, claimed.ID); !reflect.DeepEqual(first, got) {
		t.Fatalf("identical repeat changed the record: %+v -> %+v", first, got)
	}

	testutil.Call(t, testHandler.ReportAppliedSettings, postApplied(claimed.ID, report(runtimeID, generation, "m2"))).Want(http.StatusConflict)
	if got := readApplied(t, claimed.ID); !reflect.DeepEqual(first, got) {
		t.Fatalf("different repeat changed the record: %+v -> %+v", first, got)
	}
}

// A4: a reclaim after an applied report starts the new generation with no applied values.
func TestAppliedSettings_A4_ReclaimClearsApplied(t *testing.T) {
	requireDB(t)
	taskID, runtimeID, daemonID, _, _ := agedClaim(t, false)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	generation := task.DispatchedAt.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	testutil.Call(t, testHandler.ReportAppliedSettings, postApplied(taskID, report(runtimeID, generation, "m1"))).Want(http.StatusOK)
	if a := readApplied(t, taskID); a.isEmpty() {
		t.Fatal("report was not stored")
	}

	// Age this generation too so the next claim reclaims it.
	dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at = now() - interval '2 hours', prepare_lease_expires_at = now() - interval '1 minute' WHERE id = $1`, taskID)
	reclaimed := claimForkTask(t, runtimeID, daemonID)

	if reclaimed.ID != taskID {
		t.Fatalf("reclaimed task %s, want %s", reclaimed.ID, taskID)
	}
	if a := readApplied(t, taskID); !a.isEmpty() {
		t.Fatalf("applied after reclaim = %+v, want all NULL", a)
	}
}

// A5: a report without runtime_id or dispatched_at is a 400; this endpoint has no legacy form.
func TestAppliedSettings_A5_MissingGenerationIs400(t *testing.T) {
	requireDB(t)
	claimed, runtimeID := claimOnce(t)
	generation := claimedGeneration(t, claimed)
	for name, body := range map[string]map[string]any{
		"no runtime_id":    {"dispatched_at": generation, "applied_model": "m"},
		"no dispatched_at": {"runtime_id": runtimeID, "applied_model": "m"},
		"neither":          {"applied_model": "m"},
		"bad generation":   {"runtime_id": runtimeID, "dispatched_at": "yesterday", "applied_model": "m"},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.ReportAppliedSettings, postApplied(claimed.ID, body)).Want(http.StatusBadRequest)
		})
	}
	if a := readApplied(t, claimed.ID); !a.isEmpty() {
		t.Fatalf("applied = %+v, want all NULL", a)
	}
}

// A report for a task that is no longer in a startable state is refused.
func TestAppliedSettings_FinishedTaskIsRefused(t *testing.T) {
	requireDB(t)
	claimed, runtimeID := claimOnce(t)
	generation := claimedGeneration(t, claimed)
	finishTask(t, claimed.ID)

	testutil.Call(t, testHandler.ReportAppliedSettings, postApplied(claimed.ID, report(runtimeID, generation, "m"))).Want(http.StatusConflict)
	if a := readApplied(t, claimed.ID); !a.isEmpty() {
		t.Fatalf("applied = %+v, want all NULL", a)
	}
}
