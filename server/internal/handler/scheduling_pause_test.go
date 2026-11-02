package handler

import (
	"net/http"
	"sync"
	"testing"

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

func TestSchedulingPauseEventsOncePerRealChange(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	for _, s := range pauseScopes() {
		for _, typ := range []string{s.pEvent, s.rEvent} {
			testHandler.Bus.Subscribe(typ, func(e events.Event) {
				mu.Lock()
				defer mu.Unlock()
				seen[e.Type+"|"+e.WorkspaceID]++
			})
		}
	}
	count := func(typ string) int {
		mu.Lock()
		defer mu.Unlock()
		return seen[typ+"|"+testWorkspaceID]
	}
	for _, s := range pauseScopes() {
		id := s.target(t)
		p0, r0 := count(s.pEvent), count(s.rEvent)
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK) // no-op
		s.call(t, s.pause, "pause", id, nil).Want(http.StatusOK)
		s.call(t, s.pause, "pause", id, nil).Want(http.StatusOK) // no-op
		if got := count(s.pEvent) - p0; got != 1 {
			t.Fatalf("%s: %s published %d times, want 1", s.name, s.pEvent, got)
		}
		if got := count(s.rEvent) - r0; got != 0 {
			t.Fatalf("%s: %s published %d times before a real resume, want 0", s.name, s.rEvent, got)
		}
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK)
		s.call(t, s.resume, "resume", id, nil).Want(http.StatusOK) // no-op
		if got := count(s.rEvent) - r0; got != 1 {
			t.Fatalf("%s: %s published %d times, want 1", s.name, s.rEvent, got)
		}
	}
}
