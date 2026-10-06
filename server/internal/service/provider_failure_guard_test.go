package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type providerFailureProbeStub struct {
	evidence ProviderFailureProbeEvidence
	err      error
	calls    int
}

func (p *providerFailureProbeStub) Probe(context.Context) (ProviderFailureProbeEvidence, error) {
	p.calls++
	return p.evidence, p.err
}

func TestHTTPProviderFailureProbeUsesUnauthenticatedModelsEndpoint(t *testing.T) {
	var method, requestPath, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, requestPath, authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	probe, err := NewHTTPProviderFailureProbe(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := probe.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || requestPath != "/v1/models" || authorization != "" {
		t.Fatalf("request = %s %s authorization=%q", method, requestPath, authorization)
	}
	if evidence.StatusCode != http.StatusUnauthorized || evidence.CheckedAt.IsZero() {
		t.Fatalf("probe evidence = %+v", evidence)
	}
}

func TestHTTPProviderFailureProbeDoesNotFollowRedirects(t *testing.T) {
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "/redirected", http.StatusFound)
			return
		}
		redirected = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	probe, err := NewHTTPProviderFailureProbe(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := probe.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.StatusCode != http.StatusFound || redirected {
		t.Fatalf("probe followed redirect: evidence=%+v redirected=%t", evidence, redirected)
	}
}

func TestNewHTTPProviderFailureProbeRejectsUnsafeBaseURLs(t *testing.T) {
	for _, baseURL := range []string{
		"ftp://gateway.example",
		"https://user:secret@gateway.example",
		"https://gateway.example/v1?token=secret",
		"https://gateway.example/api",
	} {
		t.Run(baseURL, func(t *testing.T) {
			if _, err := NewHTTPProviderFailureProbe(baseURL); err == nil {
				t.Fatal("expected invalid provider recovery URL")
			}
		})
	}
}

func seedProviderFailureWakeupTask(t *testing.T, f principalFixture, wakeupID pgtype.UUID, headSHA, completedAt string) string {
	t.Helper()
	agentID := f.privateAgentOwnedBy(t, f.UserID, "provider-recovery")
	payload, err := json.Marshal(map[string]string{"wakeup_id": util.UUIDToString(wakeupID), "head_sha": headSHA})
	if err != nil {
		t.Fatal(err)
	}
	taskID := f.Task(t, agentID, testutil.Cols{
		"runtime_id":     testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agentID + "')"),
		"status":         "failed",
		"failure_reason": "agent_error.provider_server_error",
		"completed_at":   testutil.Raw(completedAt),
		"context":        testutil.Raw(fmt.Sprintf("'%s'::jsonb", payload)),
	})
	f.Cleanup(t, "DELETE FROM provider_failure_recovery WHERE failed_task_id=$1", taskID)
	return taskID
}

func TestProviderFailureRecoveryRequiresCooldownAndFresh401(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	wakeupID := dbid.NewV7()
	taskID := seedProviderFailureWakeupTask(t, f, wakeupID, "head-a", "now() - interval '59 minutes'")
	probe := &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: http.StatusOK}}
	service := &TaskService{Queries: f.q, ProviderFailureProbe: probe}
	scope := providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: wakeupID, ConditionKey: "head-a"}

	attempt, err := service.prepareProviderFailureRecovery(context.Background(), f.q, scope)
	if err != nil {
		t.Fatal(err)
	}
	if attempt == nil || !strings.Contains(attempt.SkipReason, "60-minute cooldown") || probe.calls != 0 {
		t.Fatalf("within-cooldown decision = %+v; probe calls=%d", attempt, probe.calls)
	}

	f.Exec(t, "UPDATE agent_task_queue SET completed_at=now()-interval '61 minutes' WHERE id=$1", taskID)
	attempt, err = service.prepareProviderFailureRecovery(context.Background(), f.q, scope)
	if err != nil {
		t.Fatal(err)
	}
	if attempt == nil || attempt.SkipReason == "" || attempt.ProbeStatus != http.StatusOK || probe.calls != 1 {
		t.Fatalf("unchanged condition decision = %+v; probe calls=%d", attempt, probe.calls)
	}

	probe.evidence.StatusCode = http.StatusUnauthorized
	probe.evidence.CheckedAt = time.Now().UTC()
	attempt, err = service.prepareProviderFailureRecovery(context.Background(), f.q, scope)
	if err != nil {
		t.Fatal(err)
	}
	if attempt == nil || attempt.FailedTaskID.String() != taskID || attempt.SkipReason != "" || probe.calls != 2 {
		t.Fatalf("recovered condition decision = %+v; probe calls=%d", attempt, probe.calls)
	}
}

func TestProviderFailureRecoveryIsScopedAndReservedOnceConcurrently(t *testing.T) {
	f, _ := newPrincipalFixture(t)
	wakeupID := dbid.NewV7()
	taskID := seedProviderFailureWakeupTask(t, f, wakeupID, "head-a", "now()-interval '61 minutes'")
	probe := &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: http.StatusUnauthorized}}
	service := &TaskService{Queries: f.q, ProviderFailureProbe: probe}
	scope := providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: wakeupID, ConditionKey: "head-a"}
	ctx := context.Background()

	snapshot, err := loadLatestProviderFailure(ctx, f.q, scope)
	if err != nil || snapshot == nil || util.UUIDToString(snapshot.ID) != taskID {
		t.Fatalf("matching trigger snapshot = %+v, err=%v", snapshot, err)
	}
	otherTrigger := providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: dbid.NewV7(), ConditionKey: "head-a"}
	if other, err := loadLatestProviderFailure(ctx, f.q, otherTrigger); err != nil || other != nil {
		t.Fatalf("different trigger snapshot = %+v, err=%v", other, err)
	}
	otherHead := providerFailureRecoveryScope{Kind: "issue_wakeup", TriggerID: wakeupID, ConditionKey: "head-b"}
	if other, err := loadLatestProviderFailure(ctx, f.q, otherHead); err != nil || other != nil {
		t.Fatalf("different head snapshot = %+v, err=%v", other, err)
	}
	attempt, err := service.prepareProviderFailureRecovery(ctx, f.q, scope)
	if err != nil || attempt == nil || attempt.SkipReason != "" {
		t.Fatalf("eligible attempt = %+v, err=%v", attempt, err)
	}

	start := make(chan struct{})
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func(recoveryTaskID pgtype.UUID) {
			defer workers.Done()
			<-start
			reserved, reserveErr := service.reserveProviderFailureRecovery(ctx, f.q, attempt, pgtype.UUID{}, recoveryTaskID)
			results <- reserved
			errors <- reserveErr
		}(dbid.NewV7())
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	winners := 0
	for reserved := range results {
		if reserved {
			winners++
		}
	}
	for reserveErr := range errors {
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent reservations = %d, want exactly one", winners)
	}
}

func TestAutopilotScheduleReusesEquivalentActiveRun(t *testing.T) {
	f, ownerID := newPrincipalFixture(t)
	agentID := f.privateAgentOwnedBy(t, ownerID, "active-schedule")
	autopilotID, triggerID := f.autopilotWithTrigger(t, agentID, ownerID, ownerID)
	ap, err := f.q.GetAutopilot(context.Background(), util.MustParseUUID(autopilotID))
	if err != nil {
		t.Fatal(err)
	}
	plannedAt := time.Now().UTC().Truncate(time.Second)

	first, err := f.svc.DispatchAutopilotForPlan(context.Background(), ap, util.MustParseUUID(triggerID), "schedule", nil, plannedAt)
	if err != nil {
		t.Fatal(err)
	}
	secondPlannedAt := plannedAt.Add(5 * time.Minute)
	second, err := f.svc.DispatchAutopilotForPlan(context.Background(), ap, util.MustParseUUID(triggerID), "schedule", nil, secondPlannedAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("second occurrence reused run %s", util.UUIDToString(first.ID))
	}
	if second.Status != "skipped" || !second.ReasonCode.Valid || second.ReasonCode.String != string(dispatch.ReasonAlreadyActive) {
		t.Fatalf("second occurrence = %+v, want a distinct already-active skip", second)
	}
	if !second.PlannedAt.Valid || !second.PlannedAt.Time.Equal(secondPlannedAt) {
		t.Fatalf("second occurrence planned_at = %+v, want %s", second.PlannedAt, secondPlannedAt)
	}
	var runs int
	if err := f.Pool.QueryRow(context.Background(), "SELECT count(*) FROM autopilot_run WHERE trigger_id=$1", triggerID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Fatalf("scheduled runs = %d, want a row for each of 2 occurrences", runs)
	}
}

func TestProviderFailureOwnerNoticeIsIdempotent(t *testing.T) {
	f, ownerID := newPrincipalFixture(t)
	wakeupID := dbid.NewV7()
	failedTaskID := seedProviderFailureWakeupTask(t, f, wakeupID, "head-a", "now()-interval '90 minutes'")
	recoveryTaskID := seedProviderFailureWakeupTask(t, f, wakeupID, "head-a", "now()-interval '1 minute'")
	if _, err := f.q.RecordProviderFailureRecovery(context.Background(), db.RecordProviderFailureRecoveryParams{
		FailedTaskID:     parseTestUUID(t, failedTaskID),
		TriggerKind:      "issue_wakeup",
		TriggerID:        wakeupID,
		ConditionKey:     "head-a",
		FailedAt:         pgtype.Timestamptz{Time: time.Now().UTC().Add(-90 * time.Minute), Valid: true},
		ProbeAt:          pgtype.Timestamptz{Time: time.Now().UTC().Add(-30 * time.Minute), Valid: true},
		ProbeStatus:      http.StatusUnauthorized,
		ChangedCondition: providerFailureChangedState,
		RecoveryTaskID:   parseTestUUID(t, recoveryTaskID),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := f.svc.TaskSvc.prepareProviderFailureRecovery(context.Background(), f.q, providerFailureRecoveryScope{
		Kind: "issue_wakeup", TriggerID: wakeupID, ConditionKey: "head-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempt == nil || !attempt.ReturnOwner || !strings.Contains(attempt.SkipReason, "new external condition") {
		t.Fatalf("failed recovery decision = %+v, want owner return without a new condition", attempt)
	}
	inboxEvents := 0
	f.svc.TaskSvc.Bus.Subscribe(protocol.EventInboxNew, func(events.Event) { inboxEvents++ })
	workspaceID := parseTestUUID(t, f.WorkspaceID)
	ownerIDValue := parseTestUUID(t, ownerID)
	for range 2 {
		f.svc.TaskSvc.notifyProviderFailureOwner(context.Background(), f.q, attempt, workspaceID, ownerIDValue, pgtype.UUID{})
	}
	var recipientID, noticeType, severity, title, body string
	if err := f.Pool.QueryRow(context.Background(), `SELECT recipient_id::text,type,severity,title,body FROM inbox_item WHERE id=$1`, recoveryTaskID).Scan(&recipientID, &noticeType, &severity, &title, &body); err != nil {
		t.Fatal(err)
	}
	if recipientID != ownerID || noticeType != "provider_failure_recovery_blocked" || severity != "action_required" || title == "" || !strings.Contains(body, "until its condition changes") {
		t.Fatalf("owner notice recipient=%s type=%s severity=%s title=%q body=%q", recipientID, noticeType, severity, title, body)
	}
	if inboxEvents != 1 {
		t.Fatalf("inbox events = %d, want exactly 1", inboxEvents)
	}
}

func TestAutopilotScheduleReservesOneRecoveryRun(t *testing.T) {
	f, ownerID := newPrincipalFixture(t)
	agentID := f.privateAgentOwnedBy(t, ownerID, "recovery-schedule")
	autopilotID, triggerID := f.autopilotWithTrigger(t, agentID, ownerID, ownerID)
	failedRun, err := f.q.CreateAutopilotRun(context.Background(), db.CreateAutopilotRunParams{
		ID: dbid.NewV7(), AutopilotID: parseTestUUID(t, autopilotID), TriggerID: parseTestUUID(t, triggerID),
		Source: "schedule", Status: "running", TriggerPayload: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM autopilot_run WHERE id=$1", util.UUIDToString(failedRun.ID))
	failedTaskID := f.Task(t, agentID, testutil.Cols{
		"runtime_id":       testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agentID + "')"),
		"autopilot_run_id": util.UUIDToString(failedRun.ID),
		"status":           "failed",
		"failure_reason":   "agent_error.provider_server_error",
		"completed_at":     testutil.Raw("now()-interval '61 minutes'"),
	})
	f.Cleanup(t, "DELETE FROM provider_failure_recovery WHERE failed_task_id=$1", failedTaskID)
	f.Exec(t, "UPDATE autopilot_run SET status='failed',completed_at=now() WHERE id=$1", util.UUIDToString(failedRun.ID))
	probe := &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: http.StatusUnauthorized}}
	f.svc.TaskSvc.ProviderFailureProbe = probe

	recoveryRun := f.dispatch(t, autopilotID, triggerID)
	if recoveryRun.Status != "running" || !recoveryRun.TaskID.Valid || probe.calls != 1 {
		t.Fatalf("recovery run = %+v; probe calls=%d", recoveryRun, probe.calls)
	}
	var recordedFailedTask, recordedRecoveryRun string
	var probeStatus int32
	if err := f.Pool.QueryRow(context.Background(), `SELECT failed_task_id,recovery_run_id,probe_status
		FROM provider_failure_recovery WHERE failed_task_id=$1`, failedTaskID).Scan(&recordedFailedTask, &recordedRecoveryRun, &probeStatus); err != nil {
		t.Fatal(err)
	}
	if recordedFailedTask != failedTaskID || recordedRecoveryRun != util.UUIDToString(recoveryRun.ID) || probeStatus != http.StatusUnauthorized {
		t.Fatalf("recovery receipt failed=%s run=%s status=%d", recordedFailedTask, recordedRecoveryRun, probeStatus)
	}
}

func TestRecurringIssueWakeupSkipsDuringCooldownAndRecoversOnce(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		failedAt    string
		probeStatus int
		wantTasks   int
		wantProbe   int
	}{
		{name: "cooldown skip", failedAt: "now()-interval '59 minutes'", probeStatus: http.StatusUnauthorized, wantTasks: 0, wantProbe: 0},
		{name: "fresh recovery", failedAt: "now()-interval '61 minutes'", probeStatus: http.StatusUnauthorized, wantTasks: 1, wantProbe: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f, service, issueID, agentID := wakeFixture(t)
			wakeup := wakeCreate(t, f, service, issueID, WakeupInput{
				AgentID: agentID, Kind: "every", Mode: "continuous", IntervalSeconds: 3600, Instruction: "Check current status",
			})
			failedTaskID := seedProviderFailureWakeupTask(t, f, wakeup.ID, "", testCase.failedAt)
			probe := &providerFailureProbeStub{evidence: ProviderFailureProbeEvidence{CheckedAt: time.Now().UTC(), StatusCode: testCase.probeStatus}}
			service.Tasks.ProviderFailureProbe = probe
			f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", wakeup.ID)
			wakeup, err := f.q.GetIssueWakeup(context.Background(), db.GetIssueWakeupParams{ID: wakeup.ID, WorkspaceID: wakeup.WorkspaceID})
			if err != nil {
				t.Fatal(err)
			}
			if err := service.dispatch(context.Background(), wakeup); err != nil {
				t.Fatal(err)
			}
			wakeup, err = f.q.GetIssueWakeup(context.Background(), db.GetIssueWakeupParams{ID: wakeup.ID, WorkspaceID: wakeup.WorkspaceID})
			if err != nil {
				t.Fatal(err)
			}
			var taskCount int
			if err := f.Pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue
				WHERE issue_id=$1 AND context->>'wakeup_id'=$2`, issueID, util.UUIDToString(wakeup.ID)).Scan(&taskCount); err != nil {
				t.Fatal(err)
			}
			if taskCount != testCase.wantTasks || probe.calls != testCase.wantProbe {
				t.Fatalf("tasks=%d probe calls=%d, want tasks=%d calls=%d", taskCount, probe.calls, testCase.wantTasks, testCase.wantProbe)
			}
			if testCase.wantTasks == 0 {
				var activityCount int
				if err := f.Pool.QueryRow(context.Background(), "SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_skipped'", issueID).Scan(&activityCount); err != nil {
					t.Fatal(err)
				}
				if activityCount != 1 || !wakeup.LastError.Valid || !strings.Contains(wakeup.LastError.String, "provider_failure_cooldown") {
					t.Fatalf("skip not visible: activities=%d last_error=%+v", activityCount, wakeup.LastError)
				}
				return
			}
			var recordedFailedTask, recordedRecoveryTask string
			var probeAt pgtype.Timestamptz
			var probeStatus int32
			if err := f.Pool.QueryRow(context.Background(), `SELECT failed_task_id,recovery_task_id,probe_at,probe_status
				FROM provider_failure_recovery WHERE failed_task_id=$1`, failedTaskID).Scan(&recordedFailedTask, &recordedRecoveryTask, &probeAt, &probeStatus); err != nil {
				t.Fatal(err)
			}
			if recordedFailedTask != failedTaskID || !probeAt.Valid || probeStatus != http.StatusUnauthorized || recordedRecoveryTask == "" {
				t.Fatalf("recovery receipt failed=%s task=%s probe_at=%+v status=%d", recordedFailedTask, recordedRecoveryTask, probeAt, probeStatus)
			}
		})
	}
}

func TestRecurringIssueWakeupDispatchUsesOneConnection(t *testing.T) {
	f, service, issueID, agentID := wakeFixture(t)
	wakeup := wakeCreate(t, f, service, issueID, WakeupInput{
		AgentID: agentID, Kind: "every", Mode: "continuous", IntervalSeconds: 3600, Instruction: "Check current status",
	})
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", wakeup.ID)
	wakeup, err := f.q.GetIssueWakeup(context.Background(), db.GetIssueWakeupParams{ID: wakeup.ID, WorkspaceID: wakeup.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	config := f.Pool.Config().Copy()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	originalQueries, originalTxStarter := service.Tasks.Queries, service.Tasks.TxStarter
	service.Tasks.Queries, service.Tasks.TxStarter = db.New(pool), pool
	t.Cleanup(func() {
		service.Tasks.Queries, service.Tasks.TxStarter = originalQueries, originalTxStarter
	})
	if err := service.dispatch(context.Background(), wakeup); err != nil {
		t.Fatalf("dispatch on a one-connection pool: %v", err)
	}
	var taskCount int
	if err := f.Pool.QueryRow(context.Background(), "SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_id'=$2", issueID, util.UUIDToString(wakeup.ID)).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 1 {
		t.Fatalf("wakeup tasks = %d, want 1", taskCount)
	}
}

func TestScheduledAutopilotFailureLookupFindsIssueTasks(t *testing.T) {
	f, ownerID := newPrincipalFixture(t)
	agentID := f.privateAgentOwnedBy(t, ownerID, "issue-schedule")
	autopilotID, triggerID := f.autopilotWithTrigger(t, agentID, ownerID, ownerID)
	run, err := f.q.CreateAutopilotRun(context.Background(), db.CreateAutopilotRunParams{
		ID: dbid.NewV7(), AutopilotID: parseTestUUID(t, autopilotID), TriggerID: parseTestUUID(t, triggerID),
		Source: "schedule", Status: "issue_created", TriggerPayload: []byte(`{"head_sha":"head-a"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Cleanup(t, "DELETE FROM autopilot_run WHERE id=$1", util.UUIDToString(run.ID))
	issueID := f.Issue(t, "Scheduled issue")
	if _, err := f.q.UpdateAutopilotRunIssueCreated(context.Background(), db.UpdateAutopilotRunIssueCreatedParams{
		ID: run.ID, IssueID: parseTestUUID(t, issueID),
	}); err != nil {
		t.Fatal(err)
	}
	taskID := f.Task(t, agentID, testutil.Cols{
		"issue_id":       issueID,
		"runtime_id":     testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agentID + "')"),
		"status":         "failed",
		"failure_reason": "agent_error.provider_server_error",
		"completed_at":   testutil.Raw("now()-interval '61 minutes'"),
	})
	row, err := f.q.FindLatestScheduledAutopilotTaskForTrigger(context.Background(), db.FindLatestScheduledAutopilotTaskForTriggerParams{
		TriggerID: parseTestUUID(t, triggerID), ConditionKey: "head-a",
	})
	if err != nil || util.UUIDToString(row.ID) != taskID || row.FailureReason.String != "agent_error.provider_server_error" {
		t.Fatalf("issue-task failure lookup = %+v, err=%v", row, err)
	}
	for _, scope := range []db.FindLatestScheduledAutopilotTaskForTriggerParams{
		{TriggerID: parseTestUUID(t, triggerID), ConditionKey: "head-b"},
		{TriggerID: dbid.NewV7(), ConditionKey: "head-a"},
	} {
		if _, err := f.q.FindLatestScheduledAutopilotTaskForTrigger(context.Background(), scope); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unrelated scheduled trigger/head lookup err=%v, want no rows", err)
		}
	}
}
