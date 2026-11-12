package handler

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// twoTargetIntent opens an intent that names the fixture's human (A) and a
// second member (B), then returns both user IDs and the intent ID.
func twoTargetIntent(t *testing.T, fixture intentFixture) (a, b, intentID string) {
	t.Helper()
	b = dbfx.User(t, "Second asked human", "intent-b-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": b, "role": "owner"})
	intentID, _ = openLaneIntent(t, fixture, fixture.mention()+" [@B](mention://member/"+b+")")
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1`, intentID); n != 2 {
		t.Fatalf("intent targets = %d, want 2", n)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE intent_id = $1`, intentID)
	})
	return fixture.humanID, b, intentID
}

func endTaskAndSeal(t *testing.T, fixture intentFixture, intentID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	if _, err := testHandler.SealDecisionIntent(context.Background(), parseUUID(testWorkspaceID), parseUUID(intentID)); err != nil {
		t.Fatal(err)
	}
}

func targetEventMembers(t *testing.T, intentID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT member_id::text FROM ask_guard_event WHERE intent_id = $1 ORDER BY 1`, intentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatal(err)
		}
		members = append(members, m)
	}
	return members
}

func sorted(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

func reconcileAskGuard(t *testing.T) {
	t.Helper()
	if err := testHandler.reconcileAskGuard(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestAskGuardIntentTargets: S3 selects each unconsumed target of a sealed
// intent, whether or not other targets were consumed. Each unserved member gets
// exactly one event and a served member gets none, across repeated passes.
func TestAskGuardIntentTargets(t *testing.T) {
	cases := []struct {
		name   string
		served func(a, b string) []string
		want   func(a, b string) []string
	}{
		{"none served", func(a, b string) []string { return nil }, func(a, b string) []string { return sorted(a, b) }},
		{"only A served", func(a, b string) []string { return []string{a} }, func(a, b string) []string { return sorted(b) }},
		{"only B served", func(a, b string) []string { return []string{b} }, func(a, b string) []string { return sorted(a) }},
		{"both served", func(a, b string) []string { return []string{a, b} }, func(a, b string) []string { return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setAskGuardMode(t, "block")
			fixture := newIntentFixture(t)
			a, b, intentID := twoTargetIntent(t, fixture)
			for _, member := range c.served(a, b) {
				dbfx.Exec(t, `UPDATE decision_intent_target SET consumed_at = clock_timestamp() WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, member)
			}
			endTaskAndSeal(t, fixture, intentID)
			reconcileAskGuard(t)
			reconcileAskGuard(t) // a restart or a second pass finds the same rows
			got, want := targetEventMembers(t, intentID), c.want(a, b)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("events for %v, want %v", got, want)
			}
		})
	}
}

// TestAskGuardIntentTargetOrdering: a sealed intent whose run is still going
// waits; the event appears once the run has ended, whichever commit came first.
func TestAskGuardIntentTargetOrdering(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newIntentFixture(t)
	a, b, intentID := twoTargetIntent(t, fixture)
	dbfx.Exec(t, `UPDATE decision_intent SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, intentID)
	if _, err := testHandler.SealDecisionIntent(context.Background(), parseUUID(testWorkspaceID), parseUUID(intentID)); err != nil {
		t.Fatal(err)
	}
	reconcileAskGuard(t)
	if got := targetEventMembers(t, intentID); len(got) != 0 {
		t.Fatalf("events while the run is live: %v", got)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'failed' WHERE id = $1`, fixture.taskID)
	reconcileAskGuard(t)
	if got := targetEventMembers(t, intentID); strings.Join(got, ",") != strings.Join(sorted(a, b), ",") {
		t.Fatalf("events after run end: %v", got)
	}
}

func TestAskGuardIntentTargetConcurrentPasses(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newIntentFixture(t)
	a, b, intentID := twoTargetIntent(t, fixture)
	endTaskAndSeal(t, fixture, intentID)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := testHandler.reconcileAskGuard(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := targetEventMembers(t, intentID); strings.Join(got, ",") != strings.Join(sorted(a, b), ",") {
		t.Fatalf("events after concurrent passes: %v", got)
	}
}

// TestAskGuardIntentTargetModeSnapshot: off writes nothing; warn stores warn
// events that never become recovery work; a later switch to block leaves them.
func TestAskGuardIntentTargetModeSnapshot(t *testing.T) {
	fixture := newIntentFixture(t)
	_, _, intentID := twoTargetIntent(t, fixture)
	endTaskAndSeal(t, fixture, intentID)
	reconcileAskGuard(t)
	if got := targetEventMembers(t, intentID); len(got) != 0 {
		t.Fatalf("off mode wrote events: %v", got)
	}
	setAskGuardMode(t, "warn")
	reconcileAskGuard(t)
	if n := askGuardEventCount(t, "intent_id = $1 AND mode = 'warn'", intentID); n != 2 {
		t.Fatalf("warn events = %d, want 2", n)
	}
	setAskGuardMode(t, "block")
	reconcileAskGuard(t)
	if n := askGuardEventCount(t, "intent_id = $1", intentID); n != 2 {
		t.Fatalf("switch to block replayed events: %d rows", n)
	}
	if n := askGuardEventCount(t, "intent_id = $1 AND mode = 'block'", intentID); n != 0 {
		t.Fatalf("block events = %d, want 0", n)
	}
}

// TestAskGuardRecoverableSelector: a block event is recovery work only once its
// task has ended and nothing cleared it, whichever of the two committed first.
func TestAskGuardRecoverableSelector(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newIntentFixture(t)
	g := askguard.Guard{Queries: testHandler.Queries}
	v, err := g.Hit(context.Background(), parseUUID(testWorkspaceID), askguard.Op{
		IssueID: parseUUID(fixture.issueID), TaskID: parseUUID(fixture.taskID), MemberID: parseUUID(fixture.humanID), Kind: "comment", Payload: []byte("ask"),
	})
	if err != nil || v.Event == nil {
		t.Fatalf("hit: %v %v", err, v.Event)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE id = $1`, v.Event.ID) })
	recoverable := func() bool {
		events, err := testHandler.Queries.ListAskGuardRecoverableEvents(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.ID == v.Event.ID {
				return true
			}
		}
		return false
	}
	if recoverable() {
		t.Fatal("event is recoverable while its task runs")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, fixture.taskID)
	if !recoverable() {
		t.Fatal("event is not recoverable after its task ended")
	}
	dbfx.Exec(t, `UPDATE ask_guard_event SET cleared_at = now() WHERE id = $1`, v.Event.ID)
	if recoverable() {
		t.Fatal("cleared event is still recoverable")
	}
}

// recoveringEvent writes a block event for the member on the issue, from an
// earlier task that has ended.
func recoveringEvent(t *testing.T, issueID, member string) string {
	t.Helper()
	id := dbid.NewV7()
	dbfx.Exec(t, `INSERT INTO ask_guard_event (id, workspace_id, issue_id, task_id, operation_kind, payload_sha256, mode, member_id) VALUES ($1, $2, $3, $4, 'comment', $5, 'block', $6)`,
		id, testWorkspaceID, issueID, dbid.NewV7(), uuid.NewString(), member)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM ask_guard_event WHERE id = $1`, id) })
	return uuidToString(id)
}

func eventCleared(t *testing.T, id string) bool {
	t.Helper()
	return askGuardEventCount(t, "id = $1 AND cleared_at IS NOT NULL", id) == 1
}

// TestDecisionRequestRecoversClearsOnlyTheMatchingEvent: a request created with
// --recovers clears that event for its member. An unrelated request, a request
// for another member, and another issue's event do not.
func TestDecisionRequestRecoversClearsOnlyTheMatchingEvent(t *testing.T) {
	fixture := newSingleCallFixture(t)
	other := dbfx.User(t, "Other human", "recovers-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "member"})
	forA := recoveringEvent(t, fixture.issueID, fixture.humanID)
	forOther := recoveringEvent(t, fixture.issueID, other)
	elsewhere := recoveringEvent(t, dbfx.Issue(t, "other issue", nil), fixture.humanID)

	for _, id := range []string{forOther, elsewhere} {
		body := fixture.body()
		body["recovers"] = id
		testutil.Call(t, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusBadRequest)
	}
	if eventCleared(t, forA) || eventCleared(t, forOther) || eventCleared(t, elsewhere) {
		t.Fatal("a refused request cleared an event")
	}

	plain := fixture.body() // an unrelated new request for the same member
	testutil.Call(t, testHandler.RequestDecision, fixture.call(plain)).Want(http.StatusCreated)
	if eventCleared(t, forA) {
		t.Fatal("an unrelated request cleared the event")
	}

	second := newSingleCallFixture(t)
	body := second.body()
	body["recovers"] = recoveringEvent(t, second.issueID, second.humanID)
	var created map[string]any
	testutil.Call(t, testHandler.RequestDecision, second.call(body)).Want(http.StatusCreated).JSON(&created)
	if !eventCleared(t, body["recovers"].(string)) {
		t.Fatal("the recovering request did not clear its event")
	}
	if askGuardEventCount(t, "cleared_by_request_id = $1", created["id"]) != 1 {
		t.Fatal("the clearing request is not recorded")
	}
}

// TestAskGuardCommentEventRecordsTheMentionedMember: the refused comment's event
// names the one member it mentioned, so a later --recovers can match it.
func TestAskGuardCommentEventRecordsTheMentionedMember(t *testing.T) {
	setAskGuardMode(t, "block")
	f := newAskGuardCommentFixture(t)
	f.post(t, map[string]any{"content": "Please decide: " + f.mention()}).Want(http.StatusUnprocessableEntity)
	if n := dbfx.Count(t, `SELECT count(*) FROM ask_guard_event WHERE issue_id = $1 AND member_id = $2`, f.issueID, f.humanID); n != 1 {
		t.Fatalf("events naming the mentioned member = %d, want 1", n)
	}
}

// TestDecisionRequestRecoversLeavesOtherEventsForTheSameMember: one member with
// two events, from two operations: clearing one keeps the other recoverable.
func TestDecisionRequestRecoversLeavesOtherEventsForTheSameMember(t *testing.T) {
	fixture := newSingleCallFixture(t)
	first := recoveringEvent(t, fixture.issueID, fixture.humanID)
	second := recoveringEvent(t, fixture.issueID, fixture.humanID)
	body := fixture.body()
	body["recovers"] = first
	testutil.Call(t, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)
	if !eventCleared(t, first) || eventCleared(t, second) {
		t.Fatalf("cleared first=%v second=%v, want true false", eventCleared(t, first), eventCleared(t, second))
	}
}

// TestAskGuardIntentTargetOutlivesAnyWindow: a run can end long after its
// intent was sealed, and a worker can be down for days. The unserved target
// still gets its event, including a partly consumed intent from before rollout.
func TestAskGuardIntentTargetOutlivesAnyWindow(t *testing.T) {
	setAskGuardMode(t, "block")
	fixture := newIntentFixture(t)
	a, b, intentID := twoTargetIntent(t, fixture)
	endTaskAndSeal(t, fixture, intentID)
	dbfx.Exec(t, `UPDATE decision_intent SET sealed_at = clock_timestamp() - interval '30 days' WHERE id = $1`, intentID)
	dbfx.Exec(t, `UPDATE decision_intent_target SET consumed_at = clock_timestamp() - interval '29 days' WHERE decision_intent_id = $1 AND human_user_id = $2`, intentID, a)
	reconcileAskGuard(t)
	if got := targetEventMembers(t, intentID); strings.Join(got, ",") != b {
		t.Fatalf("events for %v, want only the unserved %v", got, b)
	}
}

// TestDecisionRequestRecoversOnReplay: a same-task retry of a request is a
// replay, and --recovers on it is still validated and still clears the event.
func TestDecisionRequestRecoversOnReplay(t *testing.T) {
	fixture := newSingleCallFixture(t)
	other := dbfx.User(t, "Other human", "replay-"+uuid.NewString()+"@test.invalid")
	dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": other, "role": "member"})
	match := recoveringEvent(t, fixture.issueID, fixture.humanID)
	mismatch := recoveringEvent(t, fixture.issueID, other)

	testutil.Call(t, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated)

	bad := fixture.body()
	bad["recovers"] = mismatch
	testutil.Call(t, testHandler.RequestDecision, fixture.call(bad)).Want(http.StatusBadRequest)
	if eventCleared(t, mismatch) {
		t.Fatal("a replay cleared another person's event")
	}

	good := fixture.body()
	good["recovers"] = match
	if status := testutil.Call(t, testHandler.RequestDecision, fixture.call(good)).Code; status >= 300 {
		t.Fatalf("replay with matching recovers = %d", status)
	}
	if !eventCleared(t, match) {
		t.Fatal("a replay with --recovers left the event open")
	}
}

// TestDecisionRequestRecoversOneEventPerRequest: a request answers one event.
// Replaying it with a second event of the same member is refused and leaves the
// second event open, one after another and in parallel.
func TestDecisionRequestRecoversOneEventPerRequest(t *testing.T) {
	fixture := newSingleCallFixture(t)
	first := recoveringEvent(t, fixture.issueID, fixture.humanID)
	second := recoveringEvent(t, fixture.issueID, fixture.humanID)
	body := fixture.body()
	body["recovers"] = first
	testutil.Call(t, testHandler.RequestDecision, fixture.call(body)).Want(http.StatusCreated)

	again := fixture.body()
	again["recovers"] = first
	testutil.Call(t, testHandler.RequestDecision, fixture.call(again)).Want(http.StatusOK)
	other := fixture.body()
	other["recovers"] = second
	testutil.Call(t, testHandler.RequestDecision, fixture.call(other)).Want(http.StatusBadRequest)
	if eventCleared(t, second) {
		t.Fatal("a retry cleared a second event with the same request")
	}

	racer := newSingleCallFixture(t)
	a := recoveringEvent(t, racer.issueID, racer.humanID)
	b := recoveringEvent(t, racer.issueID, racer.humanID)
	testutil.Call(t, testHandler.RequestDecision, racer.call(racer.body())).Want(http.StatusCreated)
	var wg sync.WaitGroup
	for _, id := range []string{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			retry := racer.body()
			retry["recovers"] = id
			testutil.Call(t, testHandler.RequestDecision, racer.call(retry))
		}()
	}
	wg.Wait()
	if eventCleared(t, a) == eventCleared(t, b) {
		t.Fatalf("parallel retries cleared a=%v b=%v, want exactly one", eventCleared(t, a), eventCleared(t, b))
	}
}
