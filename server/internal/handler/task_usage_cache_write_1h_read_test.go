package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// 1-hour cache-write tokens must reach the readers that price estimates:
// `multica agent tasks` usage rows and the dashboard aggregate.
func TestAgentTaskUsageCarriesCacheWrite1h(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "AgentTaskUsage1h", []byte("[]"))
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": handlerTestRuntimeID(t), "status": "completed"})

	for _, row := range []testutil.Cols{
		{"model": "claude-fable-5-2", "cache_write_tokens": 300, "cache_write_1h_tokens": 120},
		{"model": "claude-sonnet-4-6", "cache_write_tokens": 50, "cache_write_1h_tokens": nil},
		{"model": "claude-opus-5", "cache_write_tokens": 40, "cache_write_1h_tokens": 0},
	} {
		row["task_id"], row["provider"] = task, "anthropic"
		dbfx.Insert(t, "task_usage", row)
	}

	req := withURLParam(newRequest(http.MethodGet, "/api/agents/"+agentID+"/tasks?include_usage=true", nil), "id", agentID)
	w := testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK)
	var raw []struct {
		ID    string           `json:"id"`
		Usage []map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	byModel := map[string]map[string]any{}
	for _, tk := range raw {
		if tk.ID == task {
			for _, u := range tk.Usage {
				byModel[u["model"].(string)] = u
			}
		}
	}
	if got := byModel["claude-fable-5-2"]["cache_write_1h_tokens"]; got != float64(120) {
		t.Errorf("split row cache_write_1h_tokens = %v, want 120", got)
	}
	if got, present := byModel["claude-sonnet-4-6"]["cache_write_1h_tokens"]; present {
		t.Errorf("NULL row serialises cache_write_1h_tokens = %v, want absent", got)
	}
	if got, present := byModel["claude-opus-5"]["cache_write_1h_tokens"]; !present || got != float64(0) {
		t.Errorf("real-zero row cache_write_1h_tokens = %v (present=%v), want 0 present", got, present)
	}
}

func TestDashboardUsageDailyCarriesUncostedCacheWrite1h(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const model = "claude-1h-rollup-fixture"
	agentID := createHandlerTestAgent(t, "DashboardUsage1h", []byte("[]"))
	mk := func(write int64, oneHour any, ticks any) {
		task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": handlerTestRuntimeID(t), "status": "completed"})
		dbfx.Insert(t, "task_usage", testutil.Cols{
			"task_id": task, "provider": "anthropic", "model": model,
			"cache_write_tokens": write, "cache_write_1h_tokens": oneHour, "cost_usd_ticks": ticks,
		})
	}
	mk(300, 120, nil)           // uncosted, split
	mk(50, nil, nil)            // uncosted, no split: prices as today
	mk(700, 700, int64(999999)) // provider-priced: excluded from the uncosted sums
	dbfx.Exec(t, `SELECT rollup_task_usage_hourly_window('1970-01-01'::timestamptz, now() + interval '1 hour')`)

	w := testutil.Call(t, testHandler.GetDashboardUsageDaily,
		newRequest("GET", "/api/dashboard/usage/daily?days=1&"+dashboardFixtureTZParam, nil)).Want(http.StatusOK)
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var write, write1h int64
	for _, r := range rows {
		if r["model"] == model {
			w, _ := r["uncosted_cache_write_tokens"].(float64)
			h, _ := r["uncosted_cache_write_1h_tokens"].(float64)
			write += int64(w)
			write1h += int64(h)
		}
	}
	if write != 350 || write1h != 120 {
		t.Errorf("uncosted cache write = %d (1h %d), want 350 (1h 120)", write, write1h)
	}
}
