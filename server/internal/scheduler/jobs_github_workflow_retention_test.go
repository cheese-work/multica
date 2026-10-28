package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeGitHubWorkflowExpirer struct {
	cutoff time.Time
	count  int64
	err    error
}

func (f *fakeGitHubWorkflowExpirer) DeleteExpired(_ context.Context, cutoff time.Time) (int64, error) {
	f.cutoff = cutoff
	return f.count, f.err
}

// Test 11: the job deletes with a 30-day cutoff and reports the rows removed.
func TestGitHubWorkflowRetentionUsesThirtyDayCutoff(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	expirer := &fakeGitHubWorkflowExpirer{count: 7}
	handler := makeGitHubWorkflowRetentionHandler(expirer, func() time.Time { return now })
	result, err := handler(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatalf("retention handler failed: %v", err)
	}
	if want := now.Add(-30 * 24 * time.Hour); !expirer.cutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", expirer.cutoff, want)
	}
	if result.RowsAffected != 7 {
		t.Fatalf("RowsAffected = %d, want 7", result.RowsAffected)
	}
}

func TestGitHubWorkflowRetentionSurfacesDeleteError(t *testing.T) {
	handler := makeGitHubWorkflowRetentionHandler(&fakeGitHubWorkflowExpirer{err: errors.New("synthetic delete failure")}, time.Now)
	_, err := handler(context.Background(), HandlerInput{})
	if err == nil || !strings.Contains(err.Error(), "synthetic delete failure") {
		t.Fatalf("err = %v, want the delete failure", err)
	}
}

// Test 11: the job name the scheduler registers.
func TestGitHubWorkflowRetentionJobConfiguration(t *testing.T) {
	job := GitHubWorkflowRetentionJob(nil)
	if job.Name != "github_workflow_retention" {
		t.Fatalf("job name = %q", job.Name)
	}
	if job.Cadence <= 0 || job.RunTimeout <= 0 || job.Handler == nil {
		t.Fatalf("unexpected retention job configuration: %+v", job)
	}
}
