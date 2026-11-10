package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// DM progress dispatcher (CHE-3405, plan I1.3-I1.6, I2, I3): the rows the
// capture triggers leave in dm_progress_pending become cards. Time is the
// "now" passed to Run; pending rows are stamped on a virtual clock two days in
// the past, so a newly captured row (real now) is told apart from stamped ones.

type dispatchEnv struct {
	capWorld
	d      *service.DMProgressDispatcher
	base   time.Time
	events []events.Event
	seen   []string // per published event: what the subscriber could read at publish time
}

func newDispatchEnv(t *testing.T) *dispatchEnv {
	t.Helper()
	e := &dispatchEnv{capWorld: newCapWorld(t), base: time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)}
	bus := events.New()
	var mu sync.Mutex
	bus.Subscribe(protocol.EventChatMessage, func(ev events.Event) {
		p := ev.Payload.(protocol.ChatMessagePayload)
		// Publish must follow commit: the message is readable from another connection.
		visible := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE id = $1`, p.MessageID)
		mu.Lock()
		defer mu.Unlock()
		e.events = append(e.events, ev)
		e.seen = append(e.seen, map[bool]string{true: "visible", false: "missing"}[visible == 1])
	})
	e.d = &service.DMProgressDispatcher{Pool: testPool, Queries: testHandler.Queries, Bus: bus}
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM dm_progress_delivery WHERE lineage_id = $1`, e.lane) })
	return e
}

func (e *dispatchEnv) at(min float64) time.Time {
	return e.base.Add(time.Duration(min * float64(time.Minute)))
}

func (e *dispatchEnv) run(t *testing.T, min float64) {
	t.Helper()
	if err := e.d.Run(t.Context(), e.at(min)); err != nil {
		t.Fatalf("Run at +%vm: %v", min, err)
	}
}

// status moves issue to status and stamps the row it captured at +min.
func (e *dispatchEnv) status(t *testing.T, issue, status string, min float64) {
	t.Helper()
	if _, err := testHandler.Queries.UpdateIssueStatus(t.Context(), db.UpdateIssueStatusParams{ID: uuidOf(issue), Status: status, WorkspaceID: uuidOf(testWorkspaceID)}); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE dm_progress_pending SET created_at = $2 WHERE issue_id = $1 AND created_at > now() - interval '1 minute'`, issue, e.at(min))
}

// child is an issue the lane follows through root (inherited), as a sub-issue would be.
func (e *dispatchEnv) child(t *testing.T) string {
	t.Helper()
	child := dbfx.Issue(t, "dispatch child "+t.Name())
	q := testHandler.Queries
	parent, err := q.GetIssue(t.Context(), uuidOf(e.issue))
	if err != nil {
		t.Fatal(err)
	}
	c, err := q.GetIssue(t.Context(), uuidOf(child))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.InheritProgressFollows(t.Context(), q, parent, c); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, child)
	return child
}

type sentCard struct {
	Seq     int64
	Message string
	Payload protocol.ProgressCardPayload
}

func (e *dispatchEnv) cards(t *testing.T) []sentCard {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT seq, chat_message_id::text, payload FROM dm_progress_delivery WHERE lineage_id = $1 ORDER BY seq`, e.lane)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []sentCard
	for rows.Next() {
		var c sentCard
		var raw []byte
		if err := rows.Scan(&c.Seq, &c.Message, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &c.Payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (e *dispatchEnv) wantCards(t *testing.T, n int) []sentCard {
	t.Helper()
	got := e.cards(t)
	if len(got) != n {
		t.Fatalf("cards = %d, want %d", len(got), n)
	}
	if msgs := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'progress_card'`, e.lane); msgs != n {
		t.Fatalf("progress_card messages = %d, want %d", msgs, n)
	}
	return got
}

func (e *dispatchEnv) pending(t *testing.T) int {
	return dbfx.Count(t, `SELECT count(*) FROM dm_progress_pending WHERE lineage_id = $1`, e.lane)
}

func TestDMProgressDispatch_TenOrdinaryChangesInFifteenMinutesGiveOneCard(t *testing.T) {
	e := newDispatchEnv(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued' WHERE chat_session_id = $1`, e.lane)
	tasks := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, e.lane)

	for i := 0; i < 10; i++ {
		e.status(t, e.issue, []string{"in_progress", "in_review"}[i%2], float64(i))
	}
	e.run(t, 14)
	e.wantCards(t, 0)

	e.run(t, 15)
	got := e.wantCards(t, 1)
	if got[0].Seq != 1 || len(got[0].Payload.Items) != 1 || got[0].Payload.Items[0].Status != "in_review" || got[0].Payload.Items[0].FromStatus != "todo" {
		t.Errorf("card = %+v, want seq 1 with one item todo -> in_review", got[0])
	}
	if n := e.pending(t); n != 0 {
		t.Errorf("pending rows after the card = %d, want 0 (every card flushes the lane)", n)
	}
	e.run(t, 16)
	e.wantCards(t, 1)

	var content, kind, role, status string
	dbfx.QueryRow(t, `SELECT content, message_kind, role FROM chat_message WHERE id = $1`, got[0].Message).Scan(&content, &kind, &role)
	if content != "Progress update: 1 issue" || kind != "progress_card" || role != "assistant" {
		t.Errorf("message = %q %q %q, want the titleless fallback line as an assistant progress_card", content, kind, role)
	}
	if strings.Contains(content, "dm progress") {
		t.Errorf("stored message leaks an issue title: %q", content)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`, e.lane); n != tasks {
		t.Errorf("tasks in the DM = %d, want %d: a card is a neutral insert", n, tasks)
	}
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE chat_session_id = $1`, e.lane).Scan(&status)
	if status != "queued" {
		t.Errorf("queued member input = %q, want unchanged", status)
	}
	if len(e.events) != 1 || e.seen[0] != "visible" || e.events[0].Payload.(protocol.ChatMessagePayload).MessageID != got[0].Message || e.events[0].ChatSessionOwnerID != testUserID {
		t.Errorf("publish = %+v %v, want one event after commit carrying message %s to the owner", e.events, e.seen, got[0].Message)
	}
	var lastCard time.Time
	var next int64
	dbfx.QueryRow(t, `SELECT last_card_at, next_seq FROM dm_progress_lane WHERE lineage_id = $1`, e.lane).Scan(&lastCard, &next)
	if !lastCard.Equal(e.at(15)) || next != 2 {
		t.Errorf("lane = last_card_at %v next_seq %d, want %v and 2", lastCard, next, e.at(15))
	}
}

func TestDMProgressDispatch_TenChildrenFinishingGiveOneCard(t *testing.T) {
	e := newDispatchEnv(t)
	for i := 0; i < 10; i++ {
		e.status(t, e.child(t), "done", float64(i))
	}
	e.run(t, 15)
	got := e.wantCards(t, 1)
	if len(got[0].Payload.Items) != 10 {
		t.Errorf("items = %d, want 10 children in one card", len(got[0].Payload.Items))
	}
}

func TestDMProgressDispatch_RootFlappingBlockedGivesOneImmediateCardPerHour(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "blocked", 0)
	e.run(t, 0)
	e.wantCards(t, 1) // the immediate card
	e.status(t, e.issue, "in_progress", 10)
	e.run(t, 25)
	e.wantCards(t, 2)
	e.status(t, e.issue, "blocked", 26) // captured as immediate again
	e.run(t, 30)
	e.wantCards(t, 2) // capped: waits for the ordinary 15 min deadline
	e.run(t, 41)
	e.wantCards(t, 3)
}

func TestDMProgressDispatch_ImmediateChangeWaitsForLaneGap(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	e.run(t, 15)
	e.wantCards(t, 1)
	e.status(t, e.issue, "done", 16)
	e.run(t, 17)
	e.run(t, 19.9)
	e.wantCards(t, 1)
	e.run(t, 20) // last_card_at + 5 min
	e.wantCards(t, 2)
}

func TestDMProgressDispatch_ImmediateChangeFlushesOlderRows(t *testing.T) {
	e := newDispatchEnv(t)
	other := e.child(t)
	e.status(t, other, "in_progress", 0)
	e.status(t, e.issue, "blocked", 14)
	e.run(t, 14)
	got := e.wantCards(t, 1)
	if len(got[0].Payload.Items) != 2 {
		t.Errorf("items = %d, want the immediate row and the older ordinary row in one card", len(got[0].Payload.Items))
	}
}

func TestDMProgressDispatch_NeedDecisionIsOrdinary(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "need_decision", 0)
	e.run(t, 1)
	e.wantCards(t, 0)
	e.run(t, 15)
	e.wantCards(t, 1)
}

func TestDMProgressDispatch_NetNoChangeSendsNothing(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	e.status(t, e.issue, "todo", 1)
	e.run(t, 15)
	e.wantCards(t, 0)
	if n := e.pending(t); n != 0 {
		t.Errorf("pending = %d, want the settled rows dropped", n)
	}
}

func TestDMProgressDispatch_MuteAndUnfollowRacingDispatchSendNothing(t *testing.T) {
	q := testHandler.Queries
	for name, stop := range map[string]func(t *testing.T, e *dispatchEnv){
		"mute": func(t *testing.T, e *dispatchEnv) {
			if _, err := q.SetDMProgressFollowMuted(t.Context(), db.SetDMProgressFollowMutedParams{IssueID: uuidOf(e.issue), LineageID: uuidOf(e.lane), Muted: true}); err != nil {
				t.Fatal(err)
			}
		},
		"unfollow": func(t *testing.T, e *dispatchEnv) {
			if _, err := q.UnfollowDMProgressFollow(t.Context(), db.UnfollowDMProgressFollowParams{IssueID: uuidOf(e.issue), LineageID: uuidOf(e.lane)}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDispatchEnv(t)
			e.status(t, e.issue, "in_progress", 0)
			stop(t, e)
			e.run(t, 20)
			e.wantCards(t, 0)
			if n := e.pending(t); n != 0 {
				t.Errorf("pending = %d, want the row dropped, not replayed on unmute", n)
			}
		})
	}
}

func TestDMProgressDispatch_RevokedMemberRowsAreDeletedAndLoggedWithoutContent(t *testing.T) {
	e := newDispatchEnv(t)
	user := dbfx.User(t, "Revoked", "revoked-"+e.lane[:8]+"@test.invalid")
	member := dbfx.Member(t, testWorkspaceID, user, "member")
	lane := e.dm.dm(t, user)
	issue := e.dm.agentIssue(t, e.dm.task(t, lane, user), map[string]any{"title": "revoked secret title"})
	e.status(t, issue, "in_progress", 0)
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_pending WHERE issue_id = $1`, issue); n == 0 {
		t.Fatal("no pending row captured before the member is revoked")
	}
	dbfx.Exec(t, `DELETE FROM member WHERE id = $1`, member)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	e.run(t, 20)

	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_pending WHERE issue_id = $1`, issue); n != 0 {
		t.Errorf("pending rows of the revoked member = %d, want deleted", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM dm_progress_delivery WHERE lineage_id = $1`, lane); n != 0 {
		t.Errorf("cards for the revoked member = %d, want none", n)
	}
	out := logs.String()
	if !strings.Contains(out, issue) || !strings.Contains(out, "not_visible") || strings.Contains(out, "revoked secret title") {
		t.Errorf("log = %q, want the issue id and reason code not_visible, no title", out)
	}
}

func TestDMProgressDispatch_DeliveryFailureLosesNothingAndRetriesSameSeq(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	dbfx.Exec(t, `INSERT INTO dm_progress_delivery (lineage_id, seq, workspace_id, chat_message_id, payload) VALUES ($1, 1, $2, gen_random_uuid(), '{}')`, e.lane, testWorkspaceID)
	if err := e.d.Run(t.Context(), e.at(15)); err == nil {
		t.Fatal("Run succeeded with seq 1 already delivered, want the unique violation to surface")
	}
	if n := e.pending(t); n != 1 {
		t.Errorf("pending after a failed delivery = %d, want 1 kept", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'progress_card'`, e.lane); n != 0 {
		t.Errorf("progress_card messages after rollback = %d, want 0", n)
	}
	var next int64
	dbfx.QueryRow(t, `SELECT next_seq FROM dm_progress_lane WHERE lineage_id = $1`, e.lane).Scan(&next)
	if next != 1 {
		t.Errorf("next_seq after rollback = %d, want 1", next)
	}
	dbfx.Exec(t, `DELETE FROM dm_progress_delivery WHERE lineage_id = $1`, e.lane)
	e.run(t, 16)
	if got := e.wantCards(t, 1); got[0].Seq != 1 {
		t.Errorf("retry seq = %d, want the same seq 1", got[0].Seq)
	}
}

func TestDMProgressDispatch_TwoDispatchersOnOneLaneSendOnce(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.d.Run(t.Context(), e.at(15)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e.wantCards(t, 1)
}

func TestDMProgressDispatch_NoNativeRouteKeepsRowsAndSuspendsLane(t *testing.T) {
	e := newDispatchEnv(t)
	successor := e.dm.dm(t, testUserID)
	dbfx.Exec(t, `UPDATE chat_session SET continued_into_session_id = $2 WHERE id = $1`, e.lane, successor)
	e.status(t, e.issue, "in_progress", 0)
	e.run(t, 15)
	e.wantCards(t, 0)
	if n := e.pending(t); n != 1 {
		t.Errorf("pending = %d, want kept for the router (C8)", n)
	}
	var reason pgtype.Text
	dbfx.QueryRow(t, `SELECT suspended_reason FROM dm_progress_lane WHERE lineage_id = $1`, e.lane).Scan(&reason)
	if !reason.Valid {
		t.Error("lane not suspended, want a reason")
	}
}

// An archive that commits between the dispatcher's route read and its commit
// must not lose the batch: the dispatcher reads the DM under a lock, so it
// waits for the archive and then sees the DM closed (review of d974a5e, P1).
func TestDMProgressDispatch_ArchiveRacingDispatchKeepsRows(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	archive, err := testPool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Rollback(t.Context())
	if _, err := archive.Exec(t.Context(), `UPDATE chat_session SET status = 'archived' WHERE id = $1`, e.lane); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- e.d.Run(t.Context(), e.at(15)) }()
	select {
	case <-time.After(500 * time.Millisecond): // blocked on the archived row, as it must be
	case err := <-done:
		t.Fatalf("dispatcher finished while the archive was uncommitted (err=%v): it read the DM without a lock", err)
	}
	if err := archive.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.wantCards(t, 0)
	if n := e.pending(t); n != 1 {
		t.Errorf("pending = %d, want kept for when the DM reopens", n)
	}
}

// A lane with no route still drops the rows of an issue its owner can no
// longer follow, so they are never replayed when the route returns (I3.7); the
// rows it may still send are kept and the lane is held (review of 9d4b740, P2).
func TestDMProgressDispatch_UnroutableLaneStillDropsDeniedRows(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, e.lane)
	dbfx.Exec(t, `INSERT INTO dm_progress_pending (workspace_id, lineage_id, issue_id, event_kind, created_at)
		VALUES ($1, $2, gen_random_uuid(), 'status', $3)`, testWorkspaceID, e.lane, e.at(0))
	e.run(t, 15)
	e.wantCards(t, 0)
	if n := e.pending(t); n != 1 {
		t.Errorf("pending = %d, want 1 (the unfollowed issue's row dropped, the followed row held)", n)
	}
	var reason pgtype.Text
	dbfx.QueryRow(t, `SELECT suspended_reason FROM dm_progress_lane WHERE lineage_id = $1`, e.lane).Scan(&reason)
	if !reason.Valid {
		t.Error("lane not held, want route_pending")
	}
}

// A lane's pending rows are compacted to one per (issue, kind, immediate) on
// every pass, so a backlog cannot grow the work of later passes (review of
// 9d4b740, security finding).
func TestDMProgressDispatch_UnroutableBacklogIsCompacted(t *testing.T) {
	e := newDispatchEnv(t)
	e.status(t, e.issue, "in_progress", 0)
	dbfx.Exec(t, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, e.lane)
	dbfx.Exec(t, `INSERT INTO dm_progress_pending (workspace_id, lineage_id, issue_id, event_kind, created_at)
		SELECT $1, $2, $3, 'status', $4::timestamptz + n * interval '1 second' FROM generate_series(1, 599) n`, testWorkspaceID, e.lane, e.issue, e.at(1))
	e.run(t, 15)
	if n := e.pending(t); n != 1 {
		t.Errorf("pending = %d, want 1 (the oldest row of the group)", n)
	}
	var oldest time.Time
	dbfx.QueryRow(t, `SELECT created_at FROM dm_progress_pending WHERE lineage_id = $1`, e.lane).Scan(&oldest)
	if !oldest.Equal(e.at(0)) {
		t.Errorf("kept row at %v, want the oldest at %v so the batch window is unchanged", oldest, e.at(0))
	}
}

// A card flushes the whole backlog (I2.4), and an immediate change behind a
// long run of ordinary rows still sets the due time.
func TestDMProgressDispatch_LargeBacklogFlushesInOneCardAndSeesLateImmediate(t *testing.T) {
	e := newDispatchEnv(t)
	dbfx.Exec(t, `INSERT INTO dm_progress_pending (workspace_id, lineage_id, issue_id, event_kind, created_at)
		SELECT $1, $2, $3, 'assignee', $4::timestamptz + n * interval '1 millisecond' FROM generate_series(1, 599) n`, testWorkspaceID, e.lane, e.issue, e.at(10))
	e.status(t, e.issue, "blocked", 11)
	e.run(t, 12) // the batch window of the oldest row ends at +25; the immediate row is due at +11
	e.wantCards(t, 1)
	if n := e.pending(t); n != 0 {
		t.Errorf("pending = %d, want 0 after the card", n)
	}
}
