package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type workspaceCapturePause struct {
	db.DBTX
	listed chan struct{}
	resume chan struct{}
}

func (pause *workspaceCapturePause) QueryRow(ctx context.Context, query string, arguments ...interface{}) pgx.Row {
	if strings.Contains(query, "FROM workspace w") {
		close(pause.listed)
		select {
		case <-pause.resume:
		case <-ctx.Done():
		}
	}
	return pause.DBTX.QueryRow(ctx, query, arguments...)
}

func TestPREventCaptureRaceDoesNotBlockWorkspaceDeletion(test *testing.T) {
	if testPool == nil {
		test.Skip("DATABASE_URL not set")
	}
	fixture := newWorkspaceDeletePathFixture(test, "pr-event-ws-race")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stamp := pgtype.Timestamptz{Time: time.Unix(1_700_100_000, 0), Valid: true}
	pullRequest, err := testHandler.Queries.UpsertGitHubPullRequest(ctx, db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(fixture.victimID), InstallationID: 987655,
		RepoOwner: "race", RepoName: "pr-event-ws", PrNumber: 7,
		Title: "review", State: "open", HtmlUrl: "http://multica.test/pr/7", HeadSha: "A",
		PrCreatedAt: stamp, PrUpdatedAt: stamp,
	})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, "INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1,$2)", fixture.victimIssue, pullRequest.ID); err != nil {
		test.Fatal(err)
	}
	capture, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	var capturePID int32
	if err := capture.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&capturePID); err != nil {
		test.Fatal(err)
	}
	pause := &workspaceCapturePause{DBTX: capture, listed: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(pause.resume) })
	captured := make(chan error, 1)
	captureDone := make(chan struct{})
	go func() {
		defer close(captureDone)
		defer capture.Rollback(context.Background())
		_, captureError := ghsnapshot.CapturePREvent(ctx, db.New(pause), ghsnapshot.PREventInput{
			PRID: pullRequest.ID, Kind: ghsnapshot.PREventConflict, HeadSHA: "A", BaseSHA: "base", ObservedAt: stamp.Time,
		})
		if captureError == nil {
			captureError = capture.Commit(ctx)
		}
		captured <- captureError
	}()
	defer func() {
		cancel()
		resume()
		<-captureDone
	}()
	select {
	case <-pause.listed:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	response := httptest.NewRecorder()
	request := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+fixture.victimID, nil).WithContext(ctx), "id", fixture.victimID)
	deleted := make(chan struct{})
	go func() {
		defer close(deleted)
		testHandler.DeleteWorkspace(response, request)
	}()
	defer func() {
		cancel()
		<-deleted
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := testPool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)) AND query LIKE '-- name: DeleteWorkspacePullRequests%')`, capturePID).Scan(&blocked); err != nil {
			test.Fatal(err)
		}
		if blocked {
			test.Log("workspace handler is waiting for capture's PR lock")
			break
		}
		select {
		case <-ticker.C:
		case <-deleted:
			test.Fatalf("workspace handler finished before expected lock wait: %d %s", response.Code, response.Body.String())
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		}
	}
	resume()
	<-deleted
	captureError := <-captured
	test.Logf("capture=%v workspace deletion=%d %s", captureError, response.Code, response.Body.String())
	if response.Code != http.StatusNoContent {
		test.Fatalf("workspace deletion regressed to %d after capture race, want 204", response.Code)
	}
}
