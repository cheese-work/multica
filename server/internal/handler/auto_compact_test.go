package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// A saved token limit must beat a percent saved earlier: the patch merges into
// the stored object, so the percent has to be cleared in the same write.
func TestClearStalePct(t *testing.T) {
	for _, tc := range []struct{ name, patch, stored, want string }{
		{"token clears an older percent", `{"threshold_tokens_1m":300000}`, `{"threshold_pct":80}`, `{"threshold_pct":null,"threshold_tokens_1m":300000}`},
		{"200k token clears it too", `{"threshold_tokens_200k":150000}`, `{"threshold_pct":80}`, `{"threshold_pct":null,"threshold_tokens_200k":150000}`},
		{"patch naming a percent keeps it", `{"threshold_pct":70,"threshold_tokens_1m":300000}`, `{}`, `{"threshold_pct":70,"threshold_tokens_1m":300000}`},
		{"enabled-only patch leaves the percent", `{"enabled":false}`, `{"threshold_pct":80}`, `{"enabled":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patch := clearStalePct(json.RawMessage(tc.patch))
			var got, want map[string]any
			if err := json.Unmarshal(patch, &got); err != nil {
				t.Fatalf("patch is not JSON: %v", err)
			}
			_ = json.Unmarshal([]byte(tc.want), &want)
			if len(got) != len(want) {
				t.Fatalf("patch = %s, want %s", patch, tc.want)
			}
			for k, v := range want {
				if gv, ok := got[k]; !ok || gv != v {
					t.Fatalf("patch = %s, want %s", patch, tc.want)
				}
			}
			// What the settings merge stores must resolve to the token limit.
			stored := map[string]any{}
			_ = json.Unmarshal([]byte(tc.stored), &stored)
			for k, v := range got {
				stored[k] = v
			}
			blob, _ := json.Marshal(map[string]any{"auto_compact": stored})
			rule := service.ResolveAutoCompact(blob, nil)
			if _, named := want["threshold_tokens_1m"]; named && want["threshold_pct"] == nil && rule.ThresholdPct != 0 {
				t.Fatalf("stored %s still resolves to %d%%", blob, rule.ThresholdPct)
			}
		})
	}
}

// An existing saved percent must not outrank a token limit saved afterwards.
func TestWorkspaceTokenLimitReplacesSavedPercent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, []byte(`{"auto_compact":{"threshold_pct":80}}`), testWorkspaceID)

	request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID,
		map[string]any{"auto_compact": map[string]any{"threshold_tokens_1m": 300000}}), "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, request).Want(http.StatusOK)

	stored, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	rule := service.ResolveAutoCompact(stored.Settings, nil)
	if rule.ThresholdPct != 0 || rule.Tokens1M != 300000 {
		t.Fatalf("settings %s resolve to pct %d, 1M tokens %d; want the 300000-token limit", stored.Settings, rule.ThresholdPct, rule.Tokens1M)
	}
}
