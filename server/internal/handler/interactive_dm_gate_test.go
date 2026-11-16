package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

// CHE-5918 (v11 slice 1a): the interactive DM gate is one server check.
// Fixture: agent and runtime both owned by testUserID, flag on.

func interactiveDMCheck(t *testing.T, ctx context.Context, actor, agentID string) *interactiveDMRefusal {
	t.Helper()
	ref, err := testHandler.checkInteractiveDM(ctx, testHandler.Queries, testWorkspaceID, actor, parseUUID(agentID))
	if err != nil {
		t.Fatalf("checkInteractiveDM: %v", err)
	}
	return ref
}

func otherUser(t *testing.T) string {
	t.Helper()
	n := testSeq.Add(1)
	return dbfx.User(t, fmt.Sprintf("dm-gate-%d", n), fmt.Sprintf("dm-gate-%d@multica.test", n))
}

func wantInteractiveDMRefusal(t *testing.T, ref *interactiveDMRefusal, code string) {
	t.Helper()
	if ref == nil {
		t.Fatalf("want refusal %q, got allowed", code)
	}
	if ref.Status != 403 || ref.Code != code {
		t.Fatalf("want 403 %q, got %d %q", code, ref.Status, ref.Code)
	}
}

func TestInteractiveDMGate(t *testing.T) {
	ctx := context.Background()
	withFeatureFlag(t, testHandler, featureflags.InteractiveDM, true)
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)

	t.Run("owner of agent and runtime is allowed", func(t *testing.T) {
		if ref := interactiveDMCheck(t, ctx, testUserID, agentID); ref != nil {
			t.Fatalf("want allowed, got %+v", ref)
		}
	})

	t.Run("non-owner is refused", func(t *testing.T) {
		other := otherUser(t)
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, other, agentID), "not_agent_owner")
	})

	t.Run("ownerless runtime is refused", func(t *testing.T) {
		dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = NULL WHERE id = $1`, runtimeID)
		t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = $1 WHERE id = $2`, testUserID, runtimeID) })
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "runtime_ownerless")
	})

	t.Run("shared runtime owned by someone else is refused", func(t *testing.T) {
		other := otherUser(t)
		dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = $1, visibility = 'public' WHERE id = $2`, other, runtimeID)
		t.Cleanup(func() {
			dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = $1, visibility = 'private' WHERE id = $2`, testUserID, runtimeID)
		})
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "runtime_owner_mismatch")
	})

	t.Run("owner changed: the next action is refused", func(t *testing.T) {
		if ref := interactiveDMCheck(t, ctx, testUserID, agentID); ref != nil {
			t.Fatalf("before change: want allowed, got %+v", ref)
		}
		other := otherUser(t)
		dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, other, agentID)
		t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, testUserID, agentID) })
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "not_agent_owner")
	})

	t.Run("agent moved to another runtime is judged on the current runtime", func(t *testing.T) {
		other := otherUser(t)
		rt2 := createRuntimeGuardRuntime(t, ctx, "opencode")
		dbfx.Exec(t, `UPDATE agent_runtime SET owner_id = $1 WHERE id = $2`, other, rt2)
		dbfx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, rt2, agentID)
		t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, agentID) })
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "runtime_owner_mismatch")
	})

	t.Run("agent in another workspace is not found", func(t *testing.T) {
		ref, err := testHandler.checkInteractiveDM(ctx, testHandler.Queries, "00000000-0000-0000-0000-000000000001", testUserID, parseUUID(agentID))
		if err != nil {
			t.Fatalf("checkInteractiveDM: %v", err)
		}
		if ref == nil || ref.Status != 404 || ref.Code != "agent_not_found" {
			t.Fatalf("want 404 agent_not_found, got %+v", ref)
		}
	})
}

func TestInteractiveDMGateFlag(t *testing.T) {
	ctx := context.Background()
	agentID, _, _ := createRuntimeGuardAgent(t, ctx)

	t.Run("flag off is refused with flag_off", func(t *testing.T) {
		withFeatureFlag(t, testHandler, featureflags.InteractiveDM, false)
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "flag_off")
	})

	t.Run("flag unset is off", func(t *testing.T) {
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "flag_off")
	})

	t.Run("flag is per workspace", func(t *testing.T) {
		provider := featureflag.NewStaticProvider()
		provider.Set(featureflags.InteractiveDM, featureflag.Rule{
			Allow: []string{"00000000-0000-0000-0000-000000000001"}, AllowBy: "workspace_id",
		})
		orig := testHandler.FeatureFlags
		testHandler.FeatureFlags = featureflag.NewService(provider)
		t.Cleanup(func() { testHandler.FeatureFlags = orig })
		wantInteractiveDMRefusal(t, interactiveDMCheck(t, ctx, testUserID, agentID), "flag_off")

		provider.Set(featureflags.InteractiveDM, featureflag.Rule{Allow: []string{testWorkspaceID}, AllowBy: "workspace_id"})
		if ref := interactiveDMCheck(t, ctx, testUserID, agentID); ref != nil {
			t.Fatalf("workspace allowed: want allowed, got %+v", ref)
		}
	})

	t.Run("flag is judged on the workspace argument, not a stale context", func(t *testing.T) {
		provider := featureflag.NewStaticProvider()
		provider.Set(featureflags.InteractiveDM, featureflag.Rule{Allow: []string{testWorkspaceID}, AllowBy: "workspace_id"})
		orig := testHandler.FeatureFlags
		testHandler.FeatureFlags = featureflag.NewService(provider)
		t.Cleanup(func() { testHandler.FeatureFlags = orig })
		stale := featureflag.WithEvalContext(ctx, featureflag.EvalContext{WorkspaceID: "00000000-0000-0000-0000-000000000002"})
		if ref := interactiveDMCheck(t, stale, testUserID, agentID); ref != nil {
			t.Fatalf("want allowed, got %+v", ref)
		}
	})
}

func TestRequireInteractiveDMWrites403WithReasonCode(t *testing.T) {
	ctx := context.Background()
	withFeatureFlag(t, testHandler, featureflags.InteractiveDM, true)
	agentID, _, _ := createRuntimeGuardAgent(t, ctx)
	w := httptest.NewRecorder()
	if testHandler.requireInteractiveDM(ctx, w, testHandler.Queries, testWorkspaceID, otherUser(t), parseUUID(agentID)) {
		t.Fatal("want refused")
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if w.Code != http.StatusForbidden || body["code"] != "not_agent_owner" {
		t.Fatalf("want 403 not_agent_owner, got %d %v", w.Code, body)
	}
	if !testHandler.requireInteractiveDM(ctx, httptest.NewRecorder(), testHandler.Queries, testWorkspaceID, testUserID, parseUUID(agentID)) {
		t.Fatal("owner: want allowed")
	}
}
