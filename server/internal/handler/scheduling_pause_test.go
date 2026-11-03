package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// scheduling pause (CHE-2954 child 2): owner/admin people only, idempotent,
// one realtime event per real change.

type pauseScope struct {
	name   string
	pause  http.HandlerFunc
	resume http.HandlerFunc
	path   func(id string) string
	paused string // SQL: count paused rows for id
	pEvent string
	rEvent string
}

func pauseScopes() []pauseScope {
	return []pauseScope{
		{
			name: "agent", pause: testHandler.PauseAgentScheduling, resume: testHandler.ResumeAgentScheduling,
			path:   func(id string) string { return "/api/agents/" + id },
			paused: `SELECT count(*) FROM agent WHERE id = $1 AND scheduling_paused_at IS NOT NULL`,
			pEvent: protocol.EventAgentSchedulingPaused, rEvent: protocol.EventAgentSchedulingResumed,
		},
		{
			name: "workspace", pause: testHandler.PauseWorkspaceScheduling, resume: testHandler.ResumeWorkspaceScheduling,
			path:   func(id string) string { return "/api/workspaces/" + id },
			paused: `SELECT count(*) FROM workspace WHERE id = $1 AND scheduling_paused_at IS NOT NULL`,
			pEvent: protocol.EventWorkspaceSchedulingPaused, rEvent: protocol.EventWorkspaceSchedulingResumed,
		},
	}
}

// target returns the id the scope's route addresses; the workspace scope uses
// the shared fixture workspace and always leaves it resumed.
func (s pauseScope) target(t *testing.T) string {
	t.Helper()
	if s.name == "agent" {
		return createHandlerTestAgent(t, "PauseTarget", []byte("[]"))
	}
	t.Cleanup(func() {
		testPool.Exec(t.Context(), `UPDATE workspace SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, testWorkspaceID)
	})
	return testWorkspaceID
}

func (s pauseScope) call(t *testing.T, fn http.HandlerFunc, verb, id string, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("POST", s.path(id)+"/"+verb, nil), "id", id)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, fn, req)
}

func (s pauseScope) isPaused(t *testing.T, id string) bool {
	t.Helper()
	return dbfx.Count(t, s.paused, id) == 1
}

func asPauseUser(userID string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-User-ID", userID) }
}

type pauseState struct {
	ID                 string  `json:"id"`
	SchedulingPausedAt *string `json:"scheduling_paused_at"`
	SchedulingPausedBy *string `json:"scheduling_paused_by"`
}

func TestSchedulingPausePermissionMatrix(t *testing.T) {
	adminID := dbfx.User(t, "Pause Admin", "pause-admin@example.com")
	dbfx.Member(t, testWorkspaceID, adminID, "admin")
	memberID := dbfx.User(t, "Pause Member", "pause-member@example.com")
	dbfx.Member(t, testWorkspaceID, memberID, "member")
	outsiderID := dbfx.User(t, "Pause Outsider", "pause-outsider@example.com")
	dbfx.Member(t, dbfx.Workspace(t, "Pause Other", "pause-other"), outsiderID, "owner")
	agentID := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, agentID)

	for _, s := range pauseScopes() {
		id := s.target(t)
		for _, verb := range []struct {
			name string
			fn   http.HandlerFunc
		}{{"pause", s.pause}, {"resume", s.resume}} {
			refused := map[string]func(*http.Request){
				"plain member": asPauseUser(memberID),
				"member-owned agent with task headers": func(r *http.Request) {
					r.Header.Set("X-Agent-ID", agentID)
					r.Header.Set("X-Task-ID", taskID)
				},
				"task token": func(r *http.Request) {
					r.Header.Set("X-Actor-Source", "task_token")
					r.Header.Set("X-Agent-ID", agentID)
				},
				"cloud pat": func(r *http.Request) { r.Header.Set("X-Actor-Source", "cloud_pat") },
			}
			for _, tweak := range refused {
				s.call(t, verb.fn, verb.name, id, tweak).Want(http.StatusForbidden)
			}
			s.call(t, verb.fn, verb.name, id, asPauseUser(outsiderID)).WantOneOf(http.StatusForbidden, http.StatusNotFound)
			if s.isPaused(t, id) {
				t.Fatalf("%s %s: refused calls left the target paused", s.name, verb.name)
			}
			for _, ok := range []string{testUserID, adminID} {
				s.call(t, verb.fn, verb.name, id, asPauseUser(ok)).Want(http.StatusOK)
			}
			// leave unpaused for the next verb
			s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK)
		}
	}
}

func TestSchedulingPauseRoundTripKeepsFirstActor(t *testing.T) {
	adminID := dbfx.User(t, "Pause Admin Two", "pause-admin2@example.com")
	dbfx.Member(t, testWorkspaceID, adminID, "admin")
	for _, s := range pauseScopes() {
		id := s.target(t)
		var first, second, resumed pauseState
		s.call(t, s.pause, "pause", id, nil).Want(http.StatusOK).JSON(&first)
		if first.SchedulingPausedAt == nil || first.SchedulingPausedBy == nil || *first.SchedulingPausedBy != testUserID {
			t.Fatalf("%s: pause response = %+v, want paused by %s", s.name, first, testUserID)
		}
		if !s.isPaused(t, id) {
			t.Fatalf("%s: row not paused after pause", s.name)
		}
		s.call(t, s.pause, "pause", id, asPauseUser(adminID)).Want(http.StatusOK).JSON(&second)
		if second.SchedulingPausedBy == nil || *second.SchedulingPausedBy != testUserID ||
			second.SchedulingPausedAt == nil || *second.SchedulingPausedAt != *first.SchedulingPausedAt {
			t.Fatalf("%s: second pause = %+v, want first actor/time %+v", s.name, second, first)
		}
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK).JSON(&resumed)
		if resumed.SchedulingPausedAt != nil || resumed.SchedulingPausedBy != nil || s.isPaused(t, id) {
			t.Fatalf("%s: resume left it paused: %+v", s.name, resumed)
		}
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK) // no-op resume is 200
	}
}

// pauseEventLog counts scheduling events per type for the fixture workspace.
// The bus has no unsubscribe, so the subscription is made once per process and
// tests compare counts before and after.
var (
	pauseEventLog  = map[string]int{}
	pauseEventLast = map[string]any{}
	pauseEventMu   sync.Mutex
	pauseEventOnce sync.Once
)

func pauseEventCount(typ string) int {
	pauseEventOnce.Do(func() {
		for _, s := range pauseScopes() {
			for _, typ := range []string{s.pEvent, s.rEvent} {
				testHandler.Bus.Subscribe(typ, func(e events.Event) {
					pauseEventMu.Lock()
					defer pauseEventMu.Unlock()
					pauseEventLog[e.Type+"|"+e.WorkspaceID]++
					pauseEventLast[e.Type+"|"+e.WorkspaceID] = e.Payload
				})
			}
		}
	})
	pauseEventMu.Lock()
	defer pauseEventMu.Unlock()
	return pauseEventLog[typ+"|"+testWorkspaceID]
}

// pauseEventPayloadJSON is the JSON a WS client would receive for the last
// event of this type in the fixture workspace.
func pauseEventPayloadJSON(t *testing.T, typ string) map[string]map[string]json.RawMessage {
	t.Helper()
	pauseEventMu.Lock()
	payload := pauseEventLast[typ+"|"+testWorkspaceID]
	pauseEventMu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSchedulingPauseEventsOncePerRealChange(t *testing.T) {
	pauseEventCount("") // subscribe before any call
	for _, s := range pauseScopes() {
		id := s.target(t)
		p0, r0 := pauseEventCount(s.pEvent), pauseEventCount(s.rEvent)
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK) // no-op
		s.call(t, s.pause, "pause", id, nil).Want(http.StatusOK)
		s.call(t, s.pause, "pause", id, nil).Want(http.StatusOK) // no-op
		if got := pauseEventCount(s.pEvent) - p0; got != 1 {
			t.Fatalf("%s: %s published %d times, want 1", s.name, s.pEvent, got)
		}
		if got := pauseEventCount(s.rEvent) - r0; got != 0 {
			t.Fatalf("%s: %s published %d times before a real resume, want 0", s.name, s.rEvent, got)
		}
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK)
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK) // no-op
		if got := pauseEventCount(s.rEvent) - r0; got != 1 {
			t.Fatalf("%s: %s published %d times, want 1", s.name, s.rEvent, got)
		}
	}
}

// fireConcurrently runs fn n times at once and returns the status codes.
func fireConcurrently(n int, fn func() int) []int {
	codes := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return codes
}

// The transition is decided with the write, not from an earlier read: n callers
// racing for one transition publish one event, whichever scope.
func TestSchedulingPauseConcurrentCallsPublishOncePerTransition(t *testing.T) {
	const racers = 8
	for _, s := range pauseScopes() {
		id := s.target(t)
		do := func(fn http.HandlerFunc, verb string) func() int {
			return func() int {
				req := withURLParam(newRequest("POST", s.path(id)+"/"+verb, nil), "id", id)
				rec := httptest.NewRecorder()
				fn(rec, req)
				return rec.Code
			}
		}
		pauseEventCount("")
		for _, step := range []struct {
			verb  string
			fn    http.HandlerFunc
			event string
		}{{"pause", s.pause, s.pEvent}, {"resume", s.resume, s.rEvent}} {
			before := pauseEventCount(step.event)
			for _, code := range fireConcurrently(racers, do(step.fn, step.verb)) {
				if code != http.StatusOK {
					t.Fatalf("%s %s: status %d, want 200", s.name, step.verb, code)
				}
			}
			if got := pauseEventCount(step.event) - before; got != 1 {
				t.Fatalf("%s: %d concurrent %s calls published %s %d times, want 1", s.name, racers, step.verb, step.event, got)
			}
		}
	}
}

// The change is committed before the response is built; a failure to decorate
// the response must not swallow its event, because a retry sees a no-op.
func TestSchedulingPauseSkillLoadFailureStillPublishes(t *testing.T) {
	pauseEventCount("")
	orig := attachAgentSkillsForScheduling
	attachAgentSkillsForScheduling = func(*Handler, context.Context, *AgentResponse, pgtype.UUID) error {
		return context.DeadlineExceeded
	}
	t.Cleanup(func() { attachAgentSkillsForScheduling = orig })

	s := pauseScopes()[0]
	id := s.target(t)
	for _, step := range []struct {
		verb  string
		fn    http.HandlerFunc
		event string
	}{{"pause", s.pause, s.pEvent}, {"resume", s.resume, s.rEvent}} {
		before := pauseEventCount(step.event)
		s.call(t, step.fn, step.verb, id, nil).Want(http.StatusInternalServerError)
		if got := pauseEventCount(step.event) - before; got != 1 {
			t.Fatalf("%s failed after commit published %s %d times, want 1", step.verb, step.event, got)
		}
		if step.verb == "pause" && !s.isPaused(t, id) {
			t.Fatal("pause was not committed")
		}
		// The skill list is unknown here; the event must not claim it is empty.
		agent := pauseEventPayloadJSON(t, step.event)["agent"]
		if _, has := agent["skills"]; has {
			t.Fatalf("%s event carries skills %s although the lookup failed", step.verb, agent["skills"])
		}
		if len(agent["id"]) == 0 {
			t.Fatalf("%s event lost the agent payload: %v", step.verb, agent)
		}
	}
}
