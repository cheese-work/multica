package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The pre-v3 onboarding shim routes sit outside the workspace role middleware
// and create a workspace-visible agent and issue. A guest is limited to the
// projects shared with them, so the handlers refuse the guest role themselves;
// a plain member still bootstraps.
func TestOnboardingShimRoutesRefuseAGuest(t *testing.T) {
	fx := testutil.New(testPool, testWorkspaceID, testUserID)

	// newUser adds a user to the workspace with the given role and a runtime
	// that user owns, and returns the user's token and runtime id.
	newUser := func(t *testing.T, role string) (token, runtimeID string) {
		t.Helper()
		email := fmt.Sprintf("onboarding-%s-%d@multica.test", role, time.Now().UnixNano())
		userID := fx.User(t, "Onboarding "+role, email)
		fx.Member(t, testWorkspaceID, userID, role)
		runtimeID = fx.Insert(t, "agent_runtime", testutil.Cols{
			"workspace_id": testWorkspaceID, "owner_id": userID, "name": "onboarding " + role,
			"runtime_mode": "local", "provider": "onboarding_guest_test", "status": "online",
			"device_info": "", "metadata": "{}", "last_seen_at": time.Now(),
		})
		token, err := generateTestJWT(userID, email, "Onboarding "+role)
		if err != nil {
			t.Fatal(err)
		}
		return token, runtimeID
	}
	count := func(t *testing.T, query string) (n int) {
		t.Helper()
		if err := testPool.QueryRow(ctx(), query, testWorkspaceID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rows := func(t *testing.T) [3]int {
		return [3]int{
			count(t, `SELECT count(*) FROM agent WHERE workspace_id = $1`),
			count(t, `SELECT count(*) FROM issue WHERE workspace_id = $1`),
			count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id = $1)`),
		}
	}
	post := func(t *testing.T, path, token string, body any) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, testServer.URL+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	routes := []struct {
		name, path string
		body       func(runtimeID string) map[string]string
	}{
		{"runtime-bootstrap", "/api/me/onboarding/runtime-bootstrap", func(rt string) map[string]string {
			return map[string]string{"workspace_id": testWorkspaceID, "runtime_id": rt, "starter_prompt": "do the guest's bidding"}
		}},
		{"no-runtime-bootstrap", "/api/me/onboarding/no-runtime-bootstrap", func(string) map[string]string {
			return map[string]string{"workspace_id": testWorkspaceID}
		}},
	}

	for _, rt := range routes {
		t.Run("guest on "+rt.name+" is refused", func(t *testing.T) {
			token, runtimeID := newUser(t, "guest")
			before := rows(t)
			code, out := post(t, rt.path, token, rt.body(runtimeID))
			if code != http.StatusForbidden {
				t.Fatalf("%s as a guest = %d %v, want 403", rt.name, code, out)
			}
			if after := rows(t); after != before {
				t.Fatalf("%s wrote rows for a guest: agents, issues, tasks %v -> %v", rt.name, before, after)
			}
		})
		t.Run("member on "+rt.name+" still bootstraps", func(t *testing.T) {
			token, runtimeID := newUser(t, "member")
			code, out := post(t, rt.path, token, rt.body(runtimeID))
			if code != http.StatusOK {
				t.Fatalf("%s as a member = %d %v, want 200", rt.name, code, out)
			}
			agentID, _ := out["agent_id"].(string)
			issueID, _ := out["issue_id"].(string)
			t.Cleanup(func() {
				if agentID != "" {
					testPool.Exec(ctx(), `DELETE FROM agent_task_queue WHERE agent_id = $1`, agentID)
					testPool.Exec(ctx(), `DELETE FROM agent WHERE id = $1`, agentID)
				}
				if issueID != "" {
					testPool.Exec(ctx(), `DELETE FROM issue WHERE id = $1`, issueID)
				}
			})
			if issueID == "" {
				t.Fatalf("%s as a member returned no issue: %v", rt.name, out)
			}
		})
	}
}
