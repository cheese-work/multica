package ghrun

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	cdDeployRunID  = int64(40343069164)
	cdQualifyRunID = int64(40339614920)
)

// restObject reads a real REST response saved from the Actions API.
func restObject(test *testing.T, name string) json.RawMessage {
	test.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

// runEnvelope wraps the real REST run object the way a workflow_run delivery does.
func runEnvelope(test *testing.T, runID int64, action, status string) []byte {
	test.Helper()
	var run map[string]any
	if err := json.Unmarshal(restObject(test, "run-"+strconv.FormatInt(runID, 10)+".json"), &run); err != nil {
		test.Fatal(err)
	}
	run["status"] = status
	if status != "completed" {
		run["conclusion"] = nil
	}
	return marshal(test, map[string]any{
		"action": action, "workflow_run": run,
		"repository":   map[string]any{"name": "multica", "owner": map[string]any{"login": "Cheese-Work"}},
		"installation": map[string]any{"id": 77},
	})
}

// jobEnvelope wraps one real REST job object the way a workflow_job delivery does.
func jobEnvelope(test *testing.T, runID int64, index int, action string) []byte {
	test.Helper()
	var jobs []map[string]any
	if err := json.Unmarshal(restObject(test, "jobs-"+strconv.FormatInt(runID, 10)+".json"), &jobs); err != nil {
		test.Fatal(err)
	}
	return marshal(test, map[string]any{
		"action": action, "workflow_job": jobs[index],
		"repository":   map[string]any{"name": "multica", "owner": map[string]any{"login": "cheese-work"}},
		"installation": map[string]any{"id": 77},
	})
}

func marshal(test *testing.T, value any) []byte {
	test.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

// Test 9: fixtures built from the real cd-deploy and cd-qualification runs.
func TestParseRunEventFromRealRuns(test *testing.T) {
	cases := []struct {
		name         string
		runID        int64
		action       string
		status       string
		wantRank     int32
		wantPath     string
		wantSHA      string
		wantEvent    string
		wantConclude string
	}{
		{"cd-deploy completed", cdDeployRunID, "completed", "completed", RankCompleted,
			".github/workflows/cd-deploy.yml", "59819ba721b4d542791db013ec2bb43ed9d66503", "workflow_run", "success"},
		{"cd-deploy in progress", cdDeployRunID, "in_progress", "in_progress", RankInProgress,
			".github/workflows/cd-deploy.yml", "59819ba721b4d542791db013ec2bb43ed9d66503", "workflow_run", ""},
		{"cd-qualification requested", cdQualifyRunID, "requested", "queued", RankRequested,
			".github/workflows/cd-qualification.yml", "7f6712c77a32f4d45b51182169ab05c5a18af0f5", "push", ""},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			event, ok := ParseRunEvent(runEnvelope(test, tc.runID, tc.action, tc.status))
			if !ok {
				test.Fatal("ParseRunEvent rejected a real run delivery")
			}
			if event.RunID != tc.runID || event.RunAttempt != 1 || event.InstallationID != 77 ||
				event.RepoOwner != "Cheese-Work" || event.RepoName != "multica" {
				test.Fatalf("identity = %+v", event)
			}
			if event.StatusRank != tc.wantRank || event.Conclusion != tc.wantConclude {
				test.Fatalf("rank %d conclusion %q, want %d %q", event.StatusRank, event.Conclusion, tc.wantRank, tc.wantConclude)
			}
			if event.WorkflowPath != tc.wantPath || event.HeadSHA != tc.wantSHA || event.Event != tc.wantEvent {
				test.Fatalf("metadata = path %q sha %q event %q", event.WorkflowPath, event.HeadSHA, event.Event)
			}
			if event.HTMLURL == "" || event.DisplayTitle == "" || event.HeadBranch != "main" || event.GhUpdatedAt.IsZero() {
				test.Fatalf("missing fields = %+v", event)
			}
		})
	}
}

func TestParseJobEventFromRealJobs(test *testing.T) {
	cases := []struct {
		action    string
		wantPhase int32
		wantOK    bool
	}{
		{"waiting", PhaseWaiting, true},
		{"in_progress", PhaseInProgress, true},
		{"completed", PhaseCompleted, true},
		{"queued", 0, false},
	}
	for _, tc := range cases {
		test.Run(tc.action, func(test *testing.T) {
			event, ok := ParseJobEvent(jobEnvelope(test, cdDeployRunID, 5, tc.action))
			if ok != tc.wantOK {
				test.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if event.Phase != tc.wantPhase || event.JobID != 121642870734 || event.JobName != "deploy" ||
				event.RunID != cdDeployRunID || event.RunAttempt != 1 || event.InstallationID != 77 ||
				event.RepoOwner != "cheese-work" || event.HeadSHA != "59819ba721b4d542791db013ec2bb43ed9d66503" {
				test.Fatalf("job event = %+v", event)
			}
		})
	}
}

func TestParseRejectsUnusablePayloads(test *testing.T) {
	for name, body := range map[string]string{
		"not json":        `{`,
		"no installation": `{"action":"completed","workflow_run":{"id":1,"run_attempt":1},"repository":{"name":"r","owner":{"login":"o"}}}`,
		"no run id":       `{"action":"completed","workflow_run":{"run_attempt":1},"repository":{"name":"r","owner":{"login":"o"}},"installation":{"id":1}}`,
		"no repository":   `{"action":"completed","workflow_run":{"id":1,"run_attempt":1},"installation":{"id":1}}`,
	} {
		if _, ok := ParseRunEvent([]byte(body)); ok {
			test.Errorf("run: %s accepted", name)
		}
	}
	for name, body := range map[string]string{
		"not json":        `{`,
		"no installation": `{"action":"waiting","workflow_job":{"id":1,"run_id":2,"run_attempt":1},"repository":{"name":"r","owner":{"login":"o"}}}`,
		"no job id":       `{"action":"waiting","workflow_job":{"run_id":2,"run_attempt":1},"repository":{"name":"r","owner":{"login":"o"}},"installation":{"id":1}}`,
		"no run id":       `{"action":"waiting","workflow_job":{"id":1,"run_attempt":1},"repository":{"name":"r","owner":{"login":"o"}},"installation":{"id":1}}`,
	} {
		if _, ok := ParseJobEvent([]byte(body)); ok {
			test.Errorf("job: %s accepted", name)
		}
	}
}

// Test 9 (store half): the parsed real deliveries land in the mirror.
func TestRealRunDeliveriesLandInMirror(test *testing.T) {
	f := newFixture(test)
	run, _ := ParseRunEvent(runEnvelope(test, cdDeployRunID, "completed", "completed"))
	job, _ := ParseJobEvent(jobEnvelope(test, cdDeployRunID, 5, "completed"))
	f.applyRun(test, run)
	f.applyJob(test, job)
	stored := f.run(test, cdDeployRunID)
	if stored.StatusRank != RankCompleted || stored.Conclusion != "success" || stored.RepoOwner != "cheese-work" ||
		stored.WorkflowPath != ".github/workflows/cd-deploy.yml" || stored.HeadSha != "59819ba721b4d542791db013ec2bb43ed9d66503" {
		test.Fatalf("stored run = %+v", stored)
	}
	if got := f.jobPhase(test, cdDeployRunID, 1, 121642870734); got != PhaseCompleted {
		test.Fatalf("job phase = %d, want %d", got, PhaseCompleted)
	}
}

// Test 11: retention deletes rows older than the cutoff only.
func TestDeleteExpiredKeepsFreshRows(test *testing.T) {
	f := newFixture(test)
	f.applyRun(test, runEvent(901, 1, RankCompleted, "completed", 0))
	f.applyJob(test, jobEvent(901, 1, 9010, PhaseCompleted))
	f.applyRun(test, runEvent(902, 1, RankCompleted, "completed", 0))
	f.applyJob(test, jobEvent(902, 1, 9020, PhaseCompleted))
	old := time.Now().Add(-31 * 24 * time.Hour)
	if _, err := f.pool.Exec(context.Background(), `UPDATE github_workflow_run SET updated_at = $2 WHERE workspace_id = $1 AND run_id = 901`, f.workspaceID, old); err != nil {
		test.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE github_workflow_job SET updated_at = $2 WHERE workspace_id = $1 AND job_id = 9010`, f.workspaceID, old); err != nil {
		test.Fatal(err)
	}
	if _, err := DeleteExpired(context.Background(), db.New(f.pool), time.Now().Add(-30*24*time.Hour)); err != nil {
		test.Fatal(err)
	}
	if _, err := db.New(f.pool).GetGitHubWorkflowRun(context.Background(), db.GetGitHubWorkflowRunParams{
		WorkspaceID: f.workspaceID, RepoOwner: testOwner, RepoName: testRepo, RunID: 901,
	}); err == nil {
		test.Error("31-day-old run row survived")
	}
	if got := f.jobPhase(test, 901, 1, 9010); got != 0 {
		test.Errorf("31-day-old job row survived, phase %d", got)
	}
	if f.run(test, 902).RunID != 902 || f.jobPhase(test, 902, 1, 9020) != PhaseCompleted {
		test.Error("fresh rows were deleted")
	}
}
