package pushnotify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/pushnotify"
	dbfx "github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-1795 (N3): the push worker. Each test builds its own user, workspace and
// device, so tests neither share state nor depend on each other's rows.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	// No default URL: a missing DATABASE_URL must skip, never fall back to a shared service.
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if pool, err := pgxpool.New(context.Background(), url); err == nil && pool.Ping(context.Background()) == nil {
			testPool = pool
		}
	}
	os.Exit(m.Run())
}

type sent struct {
	Endpoint string
	Payload  []byte
}

// fakeSender stands in for the push service. status is what it answers.
type fakeSender struct {
	mu     sync.Mutex
	status int
	err    error
	delay  time.Duration
	sent   []sent
}

func (f *fakeSender) Send(_ context.Context, sub pushnotify.Subscription, payload []byte) (int, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sent{Endpoint: sub.Endpoint, Payload: payload})
	if f.err != nil {
		return 0, f.err
	}
	if f.status == 0 {
		return 201, nil
	}
	return f.status, nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type world struct {
	t         *testing.T
	fx        *dbfx.Fixture
	q         *db.Queries
	sender    *fakeSender
	worker    *pushnotify.Worker
	userID    string
	workspace string
	slug      string
	agentID   string
	sessionID string
	endpoint  string
	subID     string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if testPool == nil {
		t.Skip("database not available")
	}
	n := time.Now().UnixNano()
	root := dbfx.New(testPool, "", "")
	userID := root.User(t, "Push Worker", fmt.Sprintf("push-worker-%d@multica.ai", n))
	slug := fmt.Sprintf("push-worker-%d", n)
	workspaceID := root.Workspace(t, "Push Worker", slug, dbfx.Cols{"issue_prefix": "PUSH"})
	fx := dbfx.New(testPool, workspaceID, userID)
	fx.Member(t, workspaceID, userID, "owner")
	agentID := fx.Agent(t, "Push Agent", fx.Runtime(t, "Push Runtime"))
	sessionID := fx.ChatSession(t, agentID, dbfx.Cols{"title": "how do I rotate the production key?", "explicitly_created_at": dbfx.Raw("now()")})

	w := &world{t: t, fx: fx, q: db.New(testPool), sender: &fakeSender{}, userID: userID, workspace: workspaceID, slug: slug, agentID: agentID, sessionID: sessionID}
	w.worker = pushnotify.NewWorker(w.q, testPool, w.sender)
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM push_delivery WHERE subscription_id IN (SELECT id FROM push_subscription WHERE user_id = $1)`, userID)
		testPool.Exec(ctx, `DELETE FROM push_subscription WHERE user_id = $1`, userID)
		testPool.Exec(ctx, `DELETE FROM inbox_item WHERE workspace_id = $1`, workspaceID)
		testPool.Exec(ctx, `DELETE FROM notification_channel_preference WHERE workspace_id = $1`, workspaceID)
	})
	w.endpoint = "https://push.example.com/send/" + slug
	w.subID = w.subscribe(w.endpoint)
	return w
}

func (w *world) subscribe(endpoint string) string {
	w.t.Helper()
	return w.fx.Insert(w.t, "push_subscription", dbfx.Cols{
		"user_id": w.userID, "endpoint": endpoint, "p256dh": "p", "auth": "a",
		"session_id": "sid-" + endpoint, "session_valid_until": dbfx.Raw("now() + interval '1 hour'"),
	})
}

func (w *world) setPush(source string, enabled bool) {
	w.t.Helper()
	w.fx.Exec(w.t, `INSERT INTO notification_channel_preference (workspace_id, user_id, source, channel, enabled) VALUES ($1, $2, $3, 'push', $4)
		ON CONFLICT (workspace_id, user_id, source, channel) DO UPDATE SET enabled = EXCLUDED.enabled`, w.workspace, w.userID, source, enabled)
}

func (w *world) uuid(s string) pgtype.UUID { return pgUUID(w.t, s) }

func pgUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// dmReply is one agent reply in the DM, as dmnotify records it.
func (w *world) dmReply() db.InboxItem {
	w.t.Helper()
	item, err := w.q.UpsertDMReplyInboxItem(context.Background(), db.UpsertDMReplyInboxItemParams{
		ID: dbid.NewV7(), WorkspaceID: w.uuid(w.workspace), RecipientID: w.uuid(w.userID), Title: "how do I rotate the production key?",
		AgentID: w.uuid(w.agentID), ChatSessionID: w.uuid(w.sessionID), ChatMessageID: dbid.NewV7(),
		DedupeKey: pgtype.Text{String: "dm:" + w.sessionID, Valid: true},
	})
	if err != nil {
		w.t.Fatalf("dm reply: %v", err)
	}
	return item
}

// decisionPing is the DM-linked "mentioned" item NotifyPing writes; its body is the ask.
func (w *world) decisionPing(ask string) db.InboxItem {
	w.t.Helper()
	return w.createItem(db.CreateInboxItemParams{
		Type: "mentioned", Severity: "info", Title: "Pick a database", Body: pgtype.Text{String: ask, Valid: true},
		ChatSessionID: w.uuid(w.sessionID), ChatMessageID: dbid.NewV7(),
	})
}

func (w *world) createItem(p db.CreateInboxItemParams) db.InboxItem {
	w.t.Helper()
	p.ID, p.WorkspaceID, p.RecipientType, p.RecipientID = dbid.NewV7(), w.uuid(w.workspace), "member", w.uuid(w.userID)
	if p.Details == nil {
		p.Details = []byte(`{}`)
	}
	item, err := w.q.CreateInboxItem(context.Background(), p)
	if err != nil {
		w.t.Fatalf("create inbox item: %v", err)
	}
	return item
}

// due moves every pending delivery of this device to the past: the 10 s delay has elapsed.
func (w *world) due() {
	w.t.Helper()
	w.fx.Exec(w.t, `UPDATE push_delivery SET next_attempt_at = now() - interval '1 second' WHERE subscription_id = $1 AND sent_at IS NULL`, w.subID)
}

func (w *world) run() {
	w.t.Helper()
	if err := w.worker.ProcessOnce(context.Background()); err != nil {
		w.t.Fatalf("process pass: %v", err)
	}
}

func (w *world) deliveries() int {
	return w.fx.Count(w.t, `SELECT count(*) FROM push_delivery WHERE subscription_id = $1`, w.subID)
}

func (w *world) payload() map[string]any {
	w.t.Helper()
	if len(w.sender.sent) != 1 {
		w.t.Fatalf("sends = %d, want exactly 1", len(w.sender.sent))
	}
	var out map[string]any
	if err := json.Unmarshal(w.sender.sent[0].Payload, &out); err != nil {
		w.t.Fatalf("payload is not json: %v", err)
	}
	return out
}

func TestReadWithinTheDelayMeansNoSend(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	item := w.dmReply()

	// Still inside the 10 s window: nothing to do yet.
	w.run()
	if w.sender.count() != 0 {
		t.Fatal("sent before the delay elapsed")
	}

	// Read on another client, then the window closes.
	w.fx.Exec(t, `UPDATE inbox_item SET read = true WHERE id = $1`, item.ID)
	w.due()
	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent a push for a record that was read first")
	}
	if n := w.deliveries(); n != 0 {
		t.Fatalf("%d delivery rows left for a read record, want 0", n)
	}
}

func TestCoalescedRepliesSendOnePushWithTheLatestCount(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	for i := 0; i < 10; i++ {
		w.dmReply()
	}
	if n := w.deliveries(); n != 1 {
		t.Fatalf("delivery rows = %d after ten replies, want 1", n)
	}
	w.due()
	w.run()

	if got := w.payload()["reply_count"]; got != float64(10) {
		t.Fatalf("reply_count = %v, want 10", got)
	}
	w.due()
	w.run()
	if w.sender.count() != 1 {
		t.Fatalf("sends = %d, want the sent record not to go out twice", w.sender.count())
	}
}

func TestRevokedSessionMeansNoSend(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.fx.Exec(t, `UPDATE push_subscription SET session_valid_until = now() - interval '1 minute' WHERE id = $1`, w.subID)
	w.due()
	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent to a device whose registering session ended")
	}
}

func TestGoneSubscriptionIsDeleted(t *testing.T) {
	for _, status := range []int{404, 410} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			w := newWorld(t)
			w.setPush("dm", true)
			w.dmReply()
			w.sender.status = status
			w.due()
			w.run()

			if n := w.fx.Count(t, `SELECT count(*) FROM push_subscription WHERE id = $1`, w.subID); n != 0 {
				t.Fatalf("subscription survived a %d from the push service", status)
			}
			if n := w.deliveries(); n != 0 {
				t.Fatalf("%d delivery rows left for a deleted subscription", n)
			}
		})
	}
}

func TestFailedSendBacksOffThenGivesUp(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.sender.status = 503
	w.due()
	w.run()

	var attempts int
	var next time.Time
	var claimed pgtype.Timestamptz
	if err := testPool.QueryRow(context.Background(),
		`SELECT attempts, next_attempt_at, claimed_until FROM push_delivery WHERE subscription_id = $1`, w.subID).Scan(&attempts, &next, &claimed); err != nil {
		t.Fatalf("read delivery after a failed send: %v", err)
	}
	if attempts != 1 || time.Until(next) < 10*time.Second || claimed.Valid {
		t.Fatalf("attempts=%d next in %v claimed=%v, want 1 attempt, a later retry and a released lease", attempts, time.Until(next), claimed.Valid)
	}

	// The last allowed attempt fails too: stop retrying, keep the device.
	w.fx.Exec(t, `UPDATE push_delivery SET attempts = $2, next_attempt_at = now() - interval '1 second' WHERE subscription_id = $1`, w.subID, pushnotify.MaxAttempts-1)
	w.run()
	if n := w.deliveries(); n != 0 {
		t.Fatalf("%d delivery rows after the attempt cap, want 0", n)
	}
	if n := w.fx.Count(t, `SELECT count(*) FROM push_subscription WHERE id = $1`, w.subID); n != 1 {
		t.Fatal("a transient push-service outage deleted the subscription")
	}
}

func TestNetworkErrorIsRetried(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.sender.err = errors.New("connection reset")
	w.due()
	w.run()

	if n := w.deliveries(); n != 1 {
		t.Fatalf("delivery rows = %d after a network error, want the row kept for retry", n)
	}
}

func TestPayloadCarriesNoMessageText(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.decisionPing("Should we migrate the billing tables tonight?")
	w.due()
	w.run()

	raw := string(w.sender.sent[0].Payload)
	for _, leak := range []string{"billing tables", "Pick a database", "rotate the production key"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("payload leaks %q: %s", leak, raw)
		}
	}
	keys := []string{}
	for k := range w.payload() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "actor_name,kind,label,record_id,reply_count,url,workspace_slug"; got != want {
		t.Fatalf("payload keys = %s, want %s", got, want)
	}
	p := w.payload()
	if p["label"] != "Decision needed" || p["kind"] != "decision_request" || p["url"] != "/"+w.slug+"/chat?session="+w.sessionID || p["workspace_slug"] != w.slug {
		t.Fatalf("payload = %v", p)
	}
}

func TestDMReplyPayloadUsesFixedLabel(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	item := w.dmReply()
	w.due()
	w.run()

	p := w.payload()
	if p["label"] != "New reply" || p["kind"] != "dm_reply" || p["record_id"] != util.UUIDToString(item.ID) || p["actor_name"] != "Push Agent" {
		t.Fatalf("payload = %v", p)
	}
	if strings.Contains(string(w.sender.sent[0].Payload), "rotate the production key") {
		t.Fatal("payload carries the DM title, which is derived from the first message")
	}
}

func TestIssueRecordNamesTheIssueButNotItsBody(t *testing.T) {
	w := newWorld(t)
	w.setPush("inbox", true)
	issueID := w.fx.Issue(t, "Rotate signing keys")
	var number int
	w.fx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&number)
	w.createItem(db.CreateInboxItemParams{
		Type: "mentioned", Severity: "info", Title: "Rotate signing keys", IssueID: w.uuid(issueID),
		Body: pgtype.Text{String: "the comment said: use the old password hunter2", Valid: true},
	})
	w.due()
	w.run()

	p := w.payload()
	if want := fmt.Sprintf("PUSH-%d Rotate signing keys", number); p["label"] != want {
		t.Fatalf("label = %v, want %q", p["label"], want)
	}
	if p["url"] != fmt.Sprintf("/%s/issues/PUSH-%d", w.slug, number) {
		t.Fatalf("url = %v", p["url"])
	}
	if strings.Contains(string(w.sender.sent[0].Payload), "hunter2") {
		t.Fatal("payload carries the item body")
	}
}

func TestItemWithoutAnIssueGetsAGenericLabel(t *testing.T) {
	w := newWorld(t)
	w.setPush("inbox", true)
	w.createItem(db.CreateInboxItemParams{Type: "task_failed", Severity: "attention", Title: "Quick create failed: summarise the leaked customer list"})
	w.due()
	w.run()

	if got := w.payload()["label"]; got != "Notification" {
		t.Fatalf("label = %v, want the fixed generic label", got)
	}
}

func TestSwitchTurnedOffBeforeSendMeansNoSend(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.setPush("dm", false)
	w.due()
	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent after the push switch was turned off")
	}
}

func TestMemberWhoLeftGetsNoPush(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.fx.Exec(t, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, w.workspace, w.userID)
	w.due()
	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent to a user who is no longer a workspace member")
	}
}

func TestDMTheUserCanNoLongerReadGetsNoPush(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	stranger := w.fx.User(t, "Push Stranger", fmt.Sprintf("push-stranger-%d@multica.ai", time.Now().UnixNano()))
	w.fx.Exec(t, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, w.sessionID, stranger)
	w.due()
	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent a push for a DM the recipient cannot read")
	}
}

func TestPushOnlyDMRecordStillSends(t *testing.T) {
	w := newWorld(t)
	// in_app stays at its default (off): the record is hidden in the inbox but exists for push.
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	w.run()

	if w.sender.count() != 1 {
		t.Fatalf("sends = %d, want the push-only record to be delivered", w.sender.count())
	}
}

func TestReassignedDeviceDoesNotReceiveTheOldOwnersRecords(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.fx.Exec(t, `UPDATE push_subscription SET user_id = $2 WHERE id = $1`, w.subID, dbid.NewV7())
	w.due()
	w.run()

	if w.sender.count() != 0 {
		t.Fatal("sent the previous account's record to the device's new owner")
	}
}

func TestLeasedRowIsNotClaimedTwice(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	w.fx.Exec(t, `UPDATE push_delivery SET claimed_until = now() + interval '1 minute' WHERE subscription_id = $1`, w.subID)
	w.run()
	if w.sender.count() != 0 {
		t.Fatal("claimed a row whose lease is still held")
	}

	// The holder died: once the lease runs out another pass takes the row over.
	w.fx.Exec(t, `UPDATE push_delivery SET claimed_until = now() - interval '1 second' WHERE subscription_id = $1`, w.subID)
	w.run()
	if w.sender.count() != 1 {
		t.Fatalf("sends = %d after lease expiry, want 1", w.sender.count())
	}
}

func TestConcurrentPassesSendOnce(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	w.sender.delay = 100 * time.Millisecond

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pushnotify.NewWorker(w.q, testPool, w.sender).ProcessOnce(context.Background()); err != nil {
				t.Errorf("process pass: %v", err)
			}
		}()
	}
	wg.Wait()

	if w.sender.count() != 1 {
		t.Fatalf("sends = %d from three overlapping passes, want 1", w.sender.count())
	}
}

func TestSentRowsAreKeptBrieflyThenRemoved(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	w.run()
	if n := w.fx.Count(t, `SELECT count(*) FROM push_delivery WHERE subscription_id = $1 AND sent_at IS NOT NULL`, w.subID); n != 1 {
		t.Fatalf("sent rows = %d right after a send, want 1 kept for the retention window", n)
	}

	w.fx.Exec(t, `UPDATE push_delivery SET sent_at = now() - interval '2 hours' WHERE subscription_id = $1`, w.subID)
	w.run()
	if n := w.deliveries(); n != 0 {
		t.Fatalf("%d sent rows past retention, want 0", n)
	}
}

func TestNewReplyAfterASendIsDeliveredAgain(t *testing.T) {
	w := newWorld(t)
	w.setPush("dm", true)
	w.dmReply()
	w.due()
	w.run()

	w.dmReply()
	w.due()
	w.run()

	if w.sender.count() != 2 {
		t.Fatalf("sends = %d, want a reply after a delivered push to notify again", w.sender.count())
	}
	if got := w.sender.sent[1].Payload; !strings.Contains(string(got), `"reply_count":2`) {
		t.Fatalf("second payload = %s, want reply_count 2", got)
	}
}

func TestNoKeysMeansNoWorker(t *testing.T) {
	for _, cfg := range []pushnotify.Config{
		{},
		{PublicKey: "pub", PrivateKey: "priv"},
		{PublicKey: "pub", Subject: "mailto:ops@example.com"},
		{PrivateKey: "priv", Subject: "mailto:ops@example.com"},
	} {
		if cfg.Enabled() {
			t.Fatalf("%+v reports enabled, want push off unless all three values are set", cfg)
		}
	}
	if !(pushnotify.Config{PublicKey: "pub", PrivateKey: "priv", Subject: "mailto:ops@example.com"}).Enabled() {
		t.Fatal("complete config reports disabled")
	}
}
