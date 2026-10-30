package ghsnapshot

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type prEventWorld struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	pr      db.GithubPullRequest
	issueID string
	wsID    pgtype.UUID
}

// seedPREventWorld seeds a workspace, an issue and an open PR (head "A") linked
// to it. settings is the workspace settings JSON.
func seedPREventWorld(t *testing.T, settings string) prEventWorld {
	t.Helper()
	pool := testDBPool(t)
	q := db.New(pool)
	f := dbfx.New(pool, "", "")
	tag := randHex(t)
	f.UserID = f.User(t, "pr event", "pr-event-"+tag+"@multica.test")
	f.WorkspaceID = f.Workspace(t, "pr event", "pr-event-"+tag)
	f.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, f.WorkspaceID, settings)
	issueID := f.Issue(t, "linked issue")
	pr := seedPRForWorkspace(t, q, parseUUID(f.WorkspaceID), 987655, "ev-"+tag, 7, "A")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_pr_event WHERE pull_request_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
	})
	f.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, issueID, pr.ID)
	return prEventWorld{pool: pool, q: q, pr: pr, issueID: issueID, wsID: parseUUID(f.WorkspaceID)}
}

type storedPREvent struct {
	Kind, HeadSHA, BaseSHA, Status string
}

func (w prEventWorld) events(t *testing.T) []storedPREvent {
	t.Helper()
	rows, err := w.pool.Query(context.Background(),
		`SELECT kind, head_sha, base_sha, status FROM issue_pr_event WHERE pull_request_id=$1 ORDER BY observed_at, created_at`, w.pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []storedPREvent
	for rows.Next() {
		var e storedPREvent
		if err := rows.Scan(&e.Kind, &e.HeadSHA, &e.BaseSHA, &e.Status); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func (w prEventWorld) manager(now time.Time) *Manager {
	return &Manager{queries: w.q, pool: w.pool, now: func() time.Time { return now }}
}

func TestPREventBaseMovedRaisesConflictOnSameHead(t *testing.T) {
	w := seedPREventWorld(t, `{}`)
	ctx := context.Background()
	t0 := time.Unix(1_700_100_000, 0)

	clean := &PRSnapshot{HeadSHA: "A", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	if applied, err := w.manager(t0).applySnapshot(ctx, w.pr.ID, clean); err != nil || !applied {
		t.Fatalf("clean snapshot applied=%v err=%v", applied, err)
	}
	if got := w.events(t); len(got) != 0 {
		t.Fatalf("clean snapshot raised events: %+v", got)
	}

	// The base branch advances; the head does not.
	moved := &PRSnapshot{HeadSHA: "A", BaseSHA: "base-2", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}
	if applied, err := w.manager(t0.Add(time.Minute)).applySnapshot(ctx, w.pr.ID, moved); err != nil || !applied {
		t.Fatalf("moved-base snapshot applied=%v err=%v", applied, err)
	}
	got := w.events(t)
	want := storedPREvent{Kind: "conflict", HeadSHA: "A", BaseSHA: "base-2", Status: "accepted"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("events = %+v, want exactly [%+v]", got, want)
	}
}

func TestPREventRepeatSnapshotStoresOneEvent(t *testing.T) {
	w := seedPREventWorld(t, `{}`)
	ctx := context.Background()
	t0 := time.Unix(1_700_100_000, 0)

	clean := &PRSnapshot{HeadSHA: "A", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	conflicting := &PRSnapshot{HeadSHA: "A", BaseSHA: "base-2", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}
	steps := []*PRSnapshot{clean, conflicting, conflicting, conflicting}
	for i, snap := range steps {
		if _, err := w.manager(t0.Add(time.Duration(i)*time.Minute)).applySnapshot(ctx, w.pr.ID, snap); err != nil {
			t.Fatal(err)
		}
	}
	// The same conflict re-observed after a flap through UNKNOWN is still one event.
	unknown := &PRSnapshot{HeadSHA: "A", BaseSHA: "base-2", Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN"}
	for i, snap := range []*PRSnapshot{unknown, conflicting} {
		if _, err := w.manager(t0.Add(time.Duration(10+i)*time.Minute)).applySnapshot(ctx, w.pr.ID, snap); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.events(t); len(got) != 1 {
		t.Fatalf("events = %+v, want exactly one conflict event", got)
	}
}

func TestPREventOlderEventAfterNewerIsSuperseded(t *testing.T) {
	w := seedPREventWorld(t, `{}`)
	ctx := context.Background()
	t0 := time.Unix(1_700_100_000, 0)

	capture := func(kind PREventKind, observed time.Time) []PREventOutcome {
		t.Helper()
		out, err := CapturePREvent(ctx, w.q, PREventInput{PRID: w.pr.ID, Kind: kind, HeadSHA: "A", BaseSHA: "b", ObservedAt: observed})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := capture(PREventReady, t0.Add(time.Minute)); len(out) != 1 || out[0] != PREventAccepted {
		t.Fatalf("newer event outcome = %v, want [accepted]", out)
	}
	// An out-of-order delivery observed before the stored latest.
	if out := capture(PREventDraft, t0); len(out) != 1 || out[0] != PREventSuperseded {
		t.Fatalf("older event outcome = %v, want [superseded]", out)
	}
	got := w.events(t)
	if len(got) != 2 || got[0].Kind != "draft" || got[0].Status != "superseded" || got[1].Kind != "ready" || got[1].Status != "accepted" {
		t.Fatalf("events = %+v, want draft superseded then ready accepted", got)
	}
}

func TestPREventChecksOnOldHeadAreDropped(t *testing.T) {
	w := seedPREventWorld(t, `{}`)
	ctx := context.Background()
	// The PR moved on to head "B"; checks for head "A" arrive late.
	if _, err := w.pool.Exec(ctx, `UPDATE github_pull_request SET head_sha='B' WHERE id=$1`, w.pr.ID); err != nil {
		t.Fatal(err)
	}
	out, err := CapturePREvent(ctx, w.q, PREventInput{PRID: w.pr.ID, Kind: PREventChecksFailed, HeadSHA: "A", ObservedAt: time.Unix(1_700_100_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != PREventDropped {
		t.Fatalf("outcome = %v, want [dropped]", out)
	}
	if got := w.events(t); len(got) != 0 {
		t.Fatalf("old-head checks stored: %+v", got)
	}
}

func TestPREventSwitchOffStopsCapture(t *testing.T) {
	w := seedPREventWorld(t, `{"pr_event_capture": {"conflict": false}}`)
	ctx := context.Background()
	t0 := time.Unix(1_700_100_000, 0)

	for i, snap := range []*PRSnapshot{
		{HeadSHA: "A", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"},
		{HeadSHA: "A", BaseSHA: "base-2", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"},
	} {
		if _, err := w.manager(t0.Add(time.Duration(i)*time.Minute)).applySnapshot(ctx, w.pr.ID, snap); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.events(t); len(got) != 0 {
		t.Fatalf("conflict captured with its switch off: %+v", got)
	}
	// Only the named kind is off.
	out, err := CapturePREvent(ctx, w.q, PREventInput{PRID: w.pr.ID, Kind: PREventReady, HeadSHA: "A", ObservedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != PREventAccepted {
		t.Fatalf("other kind outcome = %v, want [accepted]", out)
	}
}

func (w prEventWorld) capture(t *testing.T, kind PREventKind, head string, observed time.Time) PREventOutcome {
	t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	outcomes, err := CapturePREvent(ctx, w.q.WithTx(tx), PREventInput{
		PRID: w.pr.ID, Kind: kind, HeadSHA: head, BaseSHA: "base", ObservedAt: observed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %v, want exactly one", outcomes)
	}
	return outcomes[0]
}

func TestPREventOldHeadObservationDoesNotSupersedeCurrentHead(t *testing.T) {
	w := seedPREventWorld(t, `{}`)
	if _, err := w.pool.Exec(context.Background(), `UPDATE github_pull_request SET head_sha='B' WHERE id=$1`, w.pr.ID); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_700_100_000, 0)
	if got := w.capture(t, PREventHeadChanged, "B", t0); got != PREventAccepted {
		t.Fatalf("current-head event = %s, want accepted", got)
	}
	if got := w.capture(t, PREventConflict, "A", t0.Add(2*time.Minute)); got != PREventSuperseded {
		t.Fatalf("old-head event = %s, want superseded", got)
	}
	if got := w.capture(t, PREventChecksFailed, "B", t0.Add(time.Minute)); got != PREventAccepted {
		t.Fatalf("current-head checks after a superseded old-head observation = %s, want accepted", got)
	}
}

func TestPREventDeletingIssueRemovesItsEvents(t *testing.T) {
	w := seedPREventWorld(t, `{}`)
	w.capture(t, PREventConflict, "A", time.Unix(1_700_100_000, 0))
	if len(w.events(t)) != 1 {
		t.Fatalf("events before delete = %+v, want one", w.events(t))
	}
	if err := w.q.DeleteIssue(context.Background(), db.DeleteIssueParams{ID: parseUUID(w.issueID), WorkspaceID: w.wsID}); err != nil {
		t.Fatal(err)
	}
	if events := w.events(t); len(events) != 0 {
		t.Fatalf("events retained after deleting their issue = %+v, want none", events)
	}
}
