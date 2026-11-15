package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5644: the audit read route and actor attribution, through the real
// router so the Auth middleware (not a test-set header) decides the actor.

func auditDo(t *testing.T, method, path string, body any, decorate func(*http.Request)) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, err := http.NewRequest(method, testServer.URL+path, &buf)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	decorate(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func bearer(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
	}
}

// liveAgentTask returns an agent of the test workspace and a running task of it.
func liveAgentTask(t *testing.T, fx *testutil.Fixture) (agentID, taskID string) {
	t.Helper()
	var runtimeID string
	fx.QueryRow(t, `SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`, testWorkspaceID).
		Scan(&agentID, &runtimeID)
	taskID = fx.Insert(t, "agent_task_queue", testutil.Cols{
		"agent_id": agentID, "runtime_id": runtimeID, "status": "running", "priority": 0,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
		"originator_source": "direct_human",
	})
	return agentID, taskID
}

func TestAuditReadRouteAccess(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agentID, taskID := liveAgentTask(t, fx)

	email := fmt.Sprintf("audit-read-route-%d@multica.test", time.Now().UnixNano())
	memberID := fx.User(t, "Audit Route Member", email)
	fx.Member(t, testWorkspaceID, memberID, "member")
	memberJWT, err := generateTestJWT(memberID, email, "Audit Route Member")
	if err != nil {
		t.Fatalf("member jwt: %v", err)
	}
	path := "/api/workspaces/" + testWorkspaceID + "/audit-events"

	for _, c := range []struct {
		name     string
		decorate func(*http.Request)
		want     int
	}{
		{"owner session", bearer(testToken), http.StatusOK},
		{"member", bearer(memberJWT), http.StatusForbidden},
		{"agent task token of the owner", bearer(mintAgentTaskToken(t, agentID, taskID, testUserID)), http.StatusForbidden},
		{"no credential", func(*http.Request) {}, http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := auditDo(t, "GET", path, nil, c.decorate)
			defer resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

type auditActorRow struct{ ActorType, ActorID, TaskID, Source string }

func latestWorkspaceUpdateAudit(t *testing.T, marker string) auditActorRow {
	t.Helper()
	var r auditActorRow
	err := testPool.QueryRow(context.Background(), `
		SELECT actor_type, COALESCE(actor_id::text, ''), COALESCE(task_id::text, ''), actor_source
		FROM workspace_audit_event
		WHERE workspace_id = $1 AND kind = 'workspace.update' AND "after" -> 'description' ->> 'len' = $2
		ORDER BY at DESC, id DESC LIMIT 1`, testWorkspaceID, fmt.Sprint(len(marker))).
		Scan(&r.ActorType, &r.ActorID, &r.TaskID, &r.Source)
	if err != nil {
		t.Fatalf("read back audit row: %v", err)
	}
	return r
}

// A forged X-Agent-ID / X-Task-ID pair on a member session must not become an
// agent row; a real mat_ token must. Both write the same audited route.
func TestAuditActorAttributionAtRouter(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)
	agentID, taskID := liveAgentTask(t, fx)
	patch := func(t *testing.T, description string, decorate func(*http.Request)) {
		t.Helper()
		resp := auditDo(t, "PATCH", "/api/workspaces/"+testWorkspaceID, map[string]any{"description": description}, decorate)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH workspace status = %d, want 200", resp.StatusCode)
		}
	}

	t.Run("forged headers on a member session", func(t *testing.T) {
		marker := strings.Repeat("f", 41+int(time.Now().UnixNano()%7))
		patch(t, marker, func(req *http.Request) {
			bearer(testToken)(req)
			req.Header.Set("X-Agent-ID", agentID)
			req.Header.Set("X-Task-ID", taskID)
			req.Header.Set("X-Actor-Source", "task_token")
		})
		got := latestWorkspaceUpdateAudit(t, marker)
		if got.ActorType != "member" || got.ActorID != testUserID || got.TaskID != "" {
			t.Fatalf("row = %+v, want member %s with no task id", got, testUserID)
		}
		if got.Source == "task_token" {
			t.Fatalf("actor_source = task_token: the forged header reached the audit row")
		}
	})

	t.Run("real task token", func(t *testing.T) {
		marker := strings.Repeat("t", 61+int(time.Now().UnixNano()%7))
		patch(t, marker, bearer(mintAgentTaskToken(t, agentID, taskID, testUserID)))
		got := latestWorkspaceUpdateAudit(t, marker)
		if got.ActorType != "agent" || got.ActorID != agentID || got.TaskID != taskID || got.Source != "task_token" {
			t.Fatalf("row = %+v, want agent %s task %s via task_token", got, agentID, taskID)
		}
	})
}

// Every route that writes an audit row must sit behind Auth or DaemonAuth,
// the middleware that strips client-supplied X-Agent-ID, X-Task-ID and
// X-Actor-Source. A route outside them would trust forged actor headers.
func TestAuditedRoutesStripActorHeaders(t *testing.T) {
	router := NewRouter(nil, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	checked := map[string]bool{}
	var unguarded []string
	err := chi.Walk(router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		if _, audited := auditedRoutes[key]; !audited {
			return nil
		}
		checked[key] = true
		for _, mw := range mws {
			name := runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name()
			if strings.Contains(name, ".Auth.") || strings.HasSuffix(name, ".Auth") || strings.Contains(name, "DaemonAuth") {
				return nil
			}
		}
		unguarded = append(unguarded, key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checked) != len(auditedRoutes) {
		t.Errorf("walked %d of %d audited routes", len(checked), len(auditedRoutes))
	}
	sort.Strings(unguarded)
	if len(unguarded) > 0 {
		t.Errorf("audited routes outside Auth/DaemonAuth:\n  %s", strings.Join(unguarded, "\n  "))
	}
}
