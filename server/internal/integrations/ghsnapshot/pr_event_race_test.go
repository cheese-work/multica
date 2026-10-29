package ghsnapshot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type pauseAfterIssueList struct {
	db.DBTX
	listed chan struct{}
	resume chan struct{}
}

func (pause *pauseAfterIssueList) QueryRow(ctx context.Context, query string, arguments ...interface{}) pgx.Row {
	if strings.Contains(query, "FROM workspace w") {
		close(pause.listed)
		select {
		case <-pause.resume:
		case <-ctx.Done():
		}
	}
	return pause.DBTX.QueryRow(ctx, query, arguments...)
}

func TestPREventConcurrentIssueDeletionDoesNotRecreateEvent(t *testing.T) {
	world := seedPREventWorld(t, `{}`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	transaction, err := world.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pause := &pauseAfterIssueList{DBTX: transaction, listed: make(chan struct{}), resume: make(chan struct{})}
	resumeCapture := sync.OnceFunc(func() { close(pause.resume) })
	completed := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(completed)
		defer transaction.Rollback(context.Background())
		_, captureError := CapturePREvent(ctx, db.New(pause), PREventInput{
			PRID: world.pr.ID, Kind: PREventConflict, HeadSHA: "A", BaseSHA: "base", ObservedAt: time.Unix(1_700_100_000, 0),
		})
		if captureError == nil {
			captureError = transaction.Commit(ctx)
		}
		result <- captureError
	}()
	defer func() {
		resumeCapture()
		<-completed
	}()
	select {
	case <-pause.listed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := world.q.DeleteIssue(ctx, db.DeleteIssueParams{ID: parseUUID(world.issueID), WorkspaceID: world.wsID}); err != nil {
		t.Fatal(err)
	}
	resumeCapture()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if events := world.events(t); len(events) != 0 {
		t.Fatalf("capture recreated events after DeleteIssue committed = %+v, want none", events)
	}
}
