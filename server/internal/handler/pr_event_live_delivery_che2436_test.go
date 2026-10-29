package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2436: a conflict or checks-failed event raised by the snapshot pipeline
// reaches a live run that holds the PR-event channel, with no manual enqueue.

var liveDeliveryPRNumber atomic.Int32

type liveDeliveryFixture struct {
	supplementFixture
	prID    pgtype.UUID
	refresh func(t *testing.T, snap ghsnapshot.PRSnapshot)
}

// newLiveDeliveryFixture links an open PR to an issue whose task has the given
// status and capability, and wires a snapshot manager whose fetcher returns the
// snapshot passed to refresh.
func newLiveDeliveryFixture(t *testing.T, taskStatus string, withChannel bool) liveDeliveryFixture {
	t.Helper()
	f := newSupplementFixture(t, "codex", taskStatus, false)
	if withChannel {
		dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
			f.taskID, testWorkspaceID, f.issueID, protocol.DaemonCapabilityTaskSupplementPREventV1)
		dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, f.taskID)
	}
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE task_id = $1`, f.taskID)

	const installationID int64 = 243601
	number := 243600 + liveDeliveryPRNumber.Add(1)
	now := time.Now().UTC()
	pr, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
		RepoOwner: "acme", RepoName: "widget", PrNumber: number, Title: "live delivery", State: "open",
		HtmlUrl:     fmt.Sprintf("https://github.com/acme/widget/pull/%d", number),
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha: "che2436-head", ClearMergeableState: pgtype.Bool{Bool: true, Valid: true}, HeadRepoSame: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM github_pull_request WHERE id = $1`, pr.ID)
	dbfx.Cleanup(t, `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, pr.ID)
	dbfx.Cleanup(t, `DELETE FROM issue_pr_event WHERE pull_request_id = $1`, pr.ID)
	if _, err := testHandler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{
		IssueID: parseUUID(f.issueID), PullRequestID: pr.ID,
	}); err != nil {
		t.Fatal(err)
	}
	setWorkspacePRWakeSetting(t, "github_enabled", true)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	client, err := ghsnapshot.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	var next atomic.Pointer[ghsnapshot.PRSnapshot]
	applied := make(chan struct{}, 4)
	manager := ghsnapshot.NewManagerWithFetcher(client, testHandler.Queries, testPool,
		func(context.Context, *ghsnapshot.Client, int64, string, string, int32) (*ghsnapshot.PRSnapshot, error) {
			return next.Load(), nil
		},
		func(ctx context.Context, prID pgtype.UUID) {
			testHandler.broadcastPRSnapshotApplied(ctx, prID)
			applied <- struct{}{}
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	t.Cleanup(cancel)

	return liveDeliveryFixture{supplementFixture: f, prID: pr.ID, refresh: func(t *testing.T, snap ghsnapshot.PRSnapshot) {
		t.Helper()
		next.Store(&snap)
		manager.Enqueue(installationID, "acme", "widget", number)
		select {
		case <-applied:
		case <-time.After(5 * time.Second):
			t.Fatal("snapshot refresh did not apply")
		}
	}}
}

func (f liveDeliveryFixture) conflicting() ghsnapshot.PRSnapshot {
	return ghsnapshot.PRSnapshot{HeadSHA: "che2436-head", BaseSHA: "base-1", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}
}

func (f liveDeliveryFixture) checksFailed() ghsnapshot.PRSnapshot {
	return ghsnapshot.PRSnapshot{HeadSHA: "che2436-head", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", HasChecks: true, RollupState: "FAILURE"}
}

func (f liveDeliveryFixture) deliveryRows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM task_pr_event_delivery WHERE task_id = $1`, f.taskID)
}

func (f liveDeliveryFixture) claim(t *testing.T) (db.ClaimNextTaskPREventRow, error) {
	t.Helper()
	return testHandler.Queries.ClaimNextTaskPREvent(context.Background(), parseUUID(f.taskID))
}

func (f liveDeliveryFixture) nextTurnKinds(t *testing.T) []string {
	t.Helper()
	rows, err := testHandler.Queries.ListNextTurnPREvents(context.Background(), db.ListNextTurnPREventsParams{
		WorkspaceIds: []pgtype.UUID{parseUUID(testWorkspaceID)}, RowLimit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, row := range rows {
		if uuidToString(row.IssueID) == f.issueID {
			kinds = append(kinds, row.Kind)
		}
	}
	return kinds
}

func TestSnapshotConflictReachesLiveRunWithPREventChannel(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	f.refresh(t, f.conflicting())
	got, err := f.claim(t)
	if err != nil {
		t.Fatalf("ClaimNextTaskPREvent: %v", err)
	}
	if got.Kind != "conflict" || got.HeadSha != "che2436-head" {
		t.Fatalf("claimed %+v, want the conflict on che2436-head", got)
	}
}

func TestSnapshotChecksFailedReachesLiveRunWithPREventChannel(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	f.refresh(t, f.checksFailed())
	got, err := f.claim(t)
	if err != nil {
		t.Fatalf("ClaimNextTaskPREvent: %v", err)
	}
	if got.Kind != "checks_failed" || got.HeadSha != "che2436-head" {
		t.Fatalf("claimed %+v, want checks_failed on che2436-head", got)
	}
}

func TestSnapshotRepeatKeepsOneDeliveryRow(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	f.refresh(t, f.conflicting())
	f.refresh(t, f.conflicting())
	if got := f.deliveryRows(t); got != 1 {
		t.Fatalf("delivery rows after a repeated snapshot = %d, want 1", got)
	}
}

func TestSnapshotEventForLegacyRunUsesNextTurnFallback(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", false)
	f.refresh(t, f.conflicting())
	if got := f.deliveryRows(t); got != 0 {
		t.Fatalf("legacy run got %d delivery rows, want 0", got)
	}
	if _, err := f.claim(t); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("legacy run claim error = %v, want no rows", err)
	}
	if kinds := f.nextTurnKinds(t); len(kinds) != 1 || kinds[0] != "conflict" {
		t.Fatalf("next-turn fallback kinds = %v, want [conflict]", kinds)
	}
}

func TestSnapshotEventWithNoLiveRunUsesNextTurnFallback(t *testing.T) {
	f := newLiveDeliveryFixture(t, "completed", true)
	f.refresh(t, f.conflicting())
	if got := f.deliveryRows(t); got != 0 {
		t.Fatalf("no live run, got %d delivery rows, want 0", got)
	}
	if kinds := f.nextTurnKinds(t); len(kinds) != 1 || kinds[0] != "conflict" {
		t.Fatalf("next-turn fallback kinds = %v, want [conflict]", kinds)
	}
}
