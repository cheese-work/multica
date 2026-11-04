package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// stallingInboxDB blocks the inbox write until its context ends, then records
// whether the claim release ran on a live context (like pgx, which refuses to
// execute on a finished context).
type stallingInboxDB struct {
	db.DBTX
	releases, released int
}

func (s *stallingInboxDB) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	<-ctx.Done()
	return errRow{ctx.Err()}
}

func (s *stallingInboxDB) Exec(ctx context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(sql, "drained_notified_at") {
		return pgconn.CommandTag{}, pgx.ErrTxClosed
	}
	s.releases++
	if err := ctx.Err(); err != nil {
		return pgconn.CommandTag{}, err
	}
	s.released++
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// A delivery that hits its own deadline must still release the claim, or the
// next heartbeat skips the undelivered notice.
func TestDeliverDrainNotice_TimeoutStillReleasesClaim(t *testing.T) {
	old := drainNoticeTimeout
	drainNoticeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { drainNoticeTimeout = old })

	fake := &stallingInboxDB{}
	h := &Handler{Queries: db.New(fake)}
	h.deliverDrainNotice(context.Background(), db.DaemonPause{
		MachineOwnerID: dbid.NewV7(), WorkspaceID: dbid.NewV7(), DaemonID: "timeout-machine", Reason: "maintenance",
		PausedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	if fake.releases != 1 || fake.released != 1 {
		t.Fatalf("release attempts = %d, succeeded = %d, want 1 and 1", fake.releases, fake.released)
	}
}
