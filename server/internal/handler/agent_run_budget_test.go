package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// An agent raises another issue's run budget inside an instance-wide daily and
// monthly cap (CHE-3251). The feature ships off; every refusal leaves the cap
// where it was.

const micro = int64(1_000_000)

// enableAgentBudget turns the feature on for one test: USD 1 a run, USD 10 a
// day, USD 100 a month, USD 200 metered limit.
func enableAgentBudget(t *testing.T, mutate ...func(*AgentRunBudgetConfig)) {
	t.Helper()
	prev := testHandler.AgentRunBudget
	cfg := AgentRunBudgetConfig{PerRunMicroUSD: micro, DailyMicroUSD: 10 * micro, MonthlyMicroUSD: 100 * micro, MeteredLimitMicroUSD: 200 * micro}
	for _, m := range mutate {
		m(&cfg)
	}
	testHandler.AgentRunBudget = cfg
	service.SetMeteredGate(cfg.MeteredGate())
	resetAgentBudgetActivity(t)
	t.Cleanup(func() {
		testHandler.AgentRunBudget = prev
		service.SetMeteredGate(prev.MeteredGate())
		resetAgentBudgetActivity(t)
	})
}

func resetAgentBudgetActivity(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM activity_log WHERE actor_type='agent' AND action IN ('run_budget_raised','run_budget_top_up')`)
}

// raisingAgent seeds a second agent with a running task: not the issue's
// assignee, so it is allowed to ask for runs on the issue.
func raisingAgent(t *testing.T) (agentID, taskID string, mutate func(*http.Request)) {
	t.Helper()
	agentID = dbfx.Agent(t, "che-3251 raiser "+time.Now().Format("150405.000000"), handlerTestRuntimeID(t))
	taskID = createHandlerTestTaskForAgent(t, agentID)
	return agentID, taskID, func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", agentID)
		r.Header.Set("X-Task-ID", taskID)
	}
}

type agentBudgetRefusal struct {
	Reason            string  `json:"reason"`
	LimitUSD          float64 `json:"limit_usd"`
	UsedUSD           float64 `json:"used_usd"`
	RemainingUSD      float64 `json:"remaining_usd"`
	RemainingRuns     int     `json:"remaining_runs"`
	RequestedRuns     int     `json:"requested_runs"`
	ResetsAt          string  `json:"resets_at"`
	Error             string  `json:"error"`
	RetryAfterSeconds string  `json:"-"`
}

func decodeRefusal(t *testing.T, w *httptest.ResponseRecorder) agentBudgetRefusal {
	t.Helper()
	var got agentBudgetRefusal
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("refusal body %q: %v", w.Body.String(), err)
	}
	got.RetryAfterSeconds = w.Header().Get("Retry-After")
	return got
}

func overrideOf(t *testing.T, issueID string) (n int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT COALESCE(max_runs_override, 0) FROM issue WHERE id=$1`, issueID).Scan(&n)
	return n
}

func budgetIssue(t *testing.T, id int64, max int) edgeCase {
	t.Helper()
	c := newEdgeCase(t, id, mergeEdgeSteps)
	spendBudget(t, c, max, "in_progress")
	return c
}

func TestAgentRaiseIsOffByDefault(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := budgetIssue(t, 3251001, 2)
	_, _, asAgent := raisingAgent(t)
	w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAgent)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled raise = %d, want 403: %s", w.Code, w.Body.String())
	}
	if got := decodeRefusal(t, w); got.Reason != "disabled" {
		t.Fatalf("reason = %q, want disabled", got.Reason)
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a disabled raise still stored an override")
	}
}

func TestAgentRaiseWithinTheCapPasses(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251002, 2)
	agentID, _, asAgent := raisingAgent(t)
	w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAgent)
	if w.Code != http.StatusOK {
		t.Fatalf("raise within the cap = %d: %s", w.Code, w.Body.String())
	}
	if overrideOf(t, c.issueID) != 5 {
		t.Fatal("the raise did not land")
	}
	var actor, runsAdded string
	dbfx.QueryRow(t, `SELECT actor_id::text, details->>'runs_added' FROM activity_log WHERE issue_id=$1 AND action='run_budget_raised'`, c.issueID).Scan(&actor, &runsAdded)
	if actor != agentID || runsAdded != "3" {
		t.Fatalf("audit actor=%s runs_added=%s, want %s and 3", actor, runsAdded, agentID)
	}
}

func TestAgentRaiseOverTheDailyCapIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t, func(c *AgentRunBudgetConfig) { c.DailyMicroUSD = 3 * micro })
	c := budgetIssue(t, 3251003, 2)
	_, _, asAgent := raisingAgent(t)
	// 2 -> 6 is 4 runs = USD 4 over a USD 3 day.
	w := raiseBudget(t, c.issueID, map[string]any{"runs": 6}, asAgent)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the daily cap = %d, want 429: %s", w.Code, w.Body.String())
	}
	got := decodeRefusal(t, w)
	if got.Reason != "daily" || got.LimitUSD != 3 || got.UsedUSD != 0 || got.RemainingUSD != 3 || got.RemainingRuns != 3 || got.RequestedRuns != 4 {
		t.Fatalf("refusal body = %+v", got)
	}
	if got.ResetsAt == "" || got.RetryAfterSeconds == "" {
		t.Fatalf("no reset time: %+v", got)
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}
	// The same agent may raise by what is left.
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAgent); w.Code != http.StatusOK {
		t.Fatalf("raise by the remaining 3 runs = %d: %s", w.Code, w.Body.String())
	}
}

func TestAgentRaiseOverTheMonthlyShareIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t, func(c *AgentRunBudgetConfig) { c.MonthlyMicroUSD = 3 * micro })
	c := budgetIssue(t, 3251004, 2)
	_, _, asAgent := raisingAgent(t)
	w := raiseBudget(t, c.issueID, map[string]any{"runs": 6}, asAgent)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the monthly share = %d, want 429: %s", w.Code, w.Body.String())
	}
	if got := decodeRefusal(t, w); got.Reason != "monthly_share" || got.LimitUSD != 3 {
		t.Fatalf("refusal body = %+v", got)
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}
}

func TestAgentRaiseCountsEarlierRaisesAndTopUps(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t, func(c *AgentRunBudgetConfig) { c.DailyMicroUSD = 6 * micro })
	c := budgetIssue(t, 3251005, 2)
	_, _, asAgent := raisingAgent(t)
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAgent); w.Code != http.StatusOK { // +3
		t.Fatalf("first raise = %d: %s", w.Code, w.Body.String())
	}
	// A top-up credits the cap in force (5 runs): USD 5 more than the USD 3 spent.
	w := topUpBudget(t, c.issueID, asAgent)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("top-up over the day = %d, want 429: %s", w.Code, w.Body.String())
	}
	if got := decodeRefusal(t, w); got.Reason != "daily" || got.UsedUSD != 3 || got.RequestedRuns != 5 {
		t.Fatalf("refusal body = %+v", got)
	}
}

func TestAgentTopUpWithinTheCapRecordsRunsAdded(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251006, 2)
	_, _, asAgent := raisingAgent(t)
	if w := topUpBudget(t, c.issueID, asAgent); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	var runsAdded string
	dbfx.QueryRow(t, `SELECT details->>'runs_added' FROM activity_log WHERE issue_id=$1 AND action='run_budget_top_up'`, c.issueID).Scan(&runsAdded)
	if runsAdded != "2" {
		t.Fatalf("top-up runs_added = %q, want 2", runsAdded)
	}
}

func TestAgentCannotRaiseItsOwnIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251007, 2) // spendBudget assigns the issue to the seeded agent
	assignee := seededReadyAgentID(t)
	taskID := createHandlerTestTaskForAgent(t, assignee)
	asAssignee := func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", assignee)
		r.Header.Set("X-Task-ID", taskID)
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAssignee); w.Code != http.StatusForbidden {
		t.Fatalf("self-raise = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := topUpBudget(t, c.issueID, asAssignee); w.Code != http.StatusForbidden {
		t.Fatalf("self top-up = %d, want 403: %s", w.Code, w.Body.String())
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}
}

func TestAgentRaiseRefusesARecoveryRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251008, 2)
	_, _, asAgent := raisingAgent(t)
	asRecovery := func(r *http.Request) {
		asAgent(r)
		*r = *r.WithContext(dispatch.WithBudgetRecoveryCaller(r.Context()))
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asRecovery); w.Code != http.StatusForbidden {
		t.Fatalf("recovery raise = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := topUpBudget(t, c.issueID, asRecovery); w.Code != http.StatusForbidden {
		t.Fatalf("recovery top-up = %d, want 403: %s", w.Code, w.Body.String())
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}
}

func TestAgentRaiseRefusesAFinishedOrForeignTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251009, 2)
	agentID, taskID, asAgent := raisingAgent(t)
	otherAgent := dbfx.Agent(t, "che-3251 other "+time.Now().Format("150405.000000"), handlerTestRuntimeID(t))
	asOther := func(r *http.Request) {
		asAgent(r)
		r.Header.Set("X-Agent-ID", otherAgent) // the task belongs to a different agent
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asOther); w.Code != http.StatusForbidden {
		t.Fatalf("task of another agent = %d, want 403: %s", w.Code, w.Body.String())
	}
	asNoTask := func(r *http.Request) {
		asAgent(r)
		r.Header.Del("X-Task-ID")
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asNoTask); w.Code != http.StatusForbidden {
		t.Fatalf("no task = %d, want 403: %s", w.Code, w.Body.String())
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, taskID)
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asAgent); w.Code != http.StatusForbidden {
		t.Fatalf("finished task of %s = %d, want 403: %s", agentID, w.Code, w.Body.String())
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}
}

func TestAgentRaiseRefusesAnAgentFromAnotherWorkspace(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251010, 2)
	otherWS := dbfx.Insert(t, "workspace", testutil.Cols{"name": "che-3251 other ws", "slug": "che-3251-" + time.Now().Format("150405000000"), "issue_prefix": "TFW"})
	rt := dbfx.Insert(t, "agent_runtime", testutil.Cols{"workspace_id": otherWS, "name": "che-3251 rt", "runtime_mode": "local", "provider": "claude", "status": "online", "device_info": "x", "metadata": "{}"})
	foreign := dbfx.Insert(t, "agent", testutil.Cols{"workspace_id": otherWS, "name": "foreign", "runtime_mode": "local", "runtime_config": "{}", "runtime_id": rt, "visibility": "workspace", "max_concurrent_tasks": 1})
	task := dbfx.Task(t, foreign, testutil.Cols{"runtime_id": rt, "status": "running", "started_at": testutil.Raw("now()")})
	asForeign := func(r *http.Request) {
		r.Header.Set("X-Actor-Source", "task_token")
		r.Header.Set("X-Agent-ID", foreign)
		r.Header.Set("X-Task-ID", task)
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, asForeign); w.Code != http.StatusForbidden {
		t.Fatalf("foreign-workspace agent = %d, want 403: %s", w.Code, w.Body.String())
	}
	if overrideOf(t, c.issueID) != 0 {
		t.Fatal("a refused raise still stored an override")
	}
}

func TestAgentRaiseNeedsATaskTokenNotBareHeaders(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t)
	c := budgetIssue(t, 3251011, 2)
	_, _, asAgent := raisingAgent(t)
	bare := func(r *http.Request) {
		asAgent(r)
		r.Header.Del("X-Actor-Source")
	}
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, bare); w.Code != http.StatusForbidden {
		t.Fatalf("agent headers without a task token = %d, want 403: %s", w.Code, w.Body.String())
	}
}

func TestMemberRaiseIsNotCapped(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t, func(c *AgentRunBudgetConfig) { c.DailyMicroUSD = micro; c.MonthlyMicroUSD = micro })
	c := budgetIssue(t, 3251012, 2)
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 50}, nil); w.Code != http.StatusOK {
		t.Fatalf("member raise past the agent cap = %d: %s", w.Code, w.Body.String())
	}
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("member top-up past the agent cap = %d: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action IN ('run_budget_raised','run_budget_top_up') AND actor_type='member'`, c.issueID); n != 2 {
		t.Fatalf("member audit rows = %d, want 2", n)
	}
}

func TestMemberRaiseWorksWhileTheAgentFeatureIsOff(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := budgetIssue(t, 3251013, 2)
	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, nil); w.Code != http.StatusOK {
		t.Fatalf("member raise = %d: %s", w.Code, w.Body.String())
	}
}

func TestParallelAgentRaisesAtTheEdgeAllowExactlyOne(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	enableAgentBudget(t, func(c *AgentRunBudgetConfig) { c.DailyMicroUSD = 3 * micro })
	a := budgetIssue(t, 3251014, 2)
	b := budgetIssue(t, 3251015, 2)
	_, _, asAgent := raisingAgent(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, issueID := range []string{a.issueID, b.issueID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = raiseBudget(t, issueID, map[string]any{"runs": 5}, asAgent).Code // +3 each, USD 3 a day
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			refused++
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("parallel raises = %v, want one 200 and one 429", codes)
	}
}

func TestICTDayAndMonthBoundaries(t *testing.T) {
	ict := time.FixedZone("ICT", 7*3600)
	// 2026-11-30 18:00 UTC is 2026-12-01 01:00 ICT: a new day and a new month.
	now := time.Date(2026, 11, 30, 18, 0, 0, 0, time.UTC)
	day, month := ictDayStart(now), ictMonthStart(now)
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, ict); !day.Equal(want) || !month.Equal(want) {
		t.Fatalf("day=%s month=%s, want %s", day, month, want)
	}
	// 2026-11-30 16:59 UTC is 23:59 ICT on the 30th.
	now = time.Date(2026, 11, 30, 16, 59, 0, 0, time.UTC)
	if want := time.Date(2026, 11, 30, 0, 0, 0, 0, ict); !ictDayStart(now).Equal(want) {
		t.Fatalf("day = %s, want %s", ictDayStart(now), want)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, ict); !ictMonthStart(now).Equal(want) {
		t.Fatalf("month = %s, want %s", ictMonthStart(now), want)
	}
	if want := time.Date(2026, 12, 1, 0, 0, 0, 0, ict); !ictNextDay(now).Equal(want) {
		t.Fatalf("next day = %s, want %s", ictNextDay(now), want)
	}
}

func TestParseAgentRunBudgetConfig(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	cfg, err := ParseAgentRunBudgetConfig(env(nil))
	if err != nil || cfg.Enabled() {
		t.Fatalf("unset = %+v, %v; want disabled, no error", cfg, err)
	}
	cfg, err = ParseAgentRunBudgetConfig(env(map[string]string{
		"MULTICA_AGENT_RUN_BUDGET_USD_PER_RUN": "0.5", "MULTICA_AGENT_RUN_BUDGET_DAILY_USD": "10",
		"MULTICA_AGENT_RUN_BUDGET_MONTHLY_USD": "100.25", "MULTICA_AGENT_RUN_BUDGET_METERED_LIMIT_USD": "150",
	}))
	if err != nil || cfg.PerRunMicroUSD != 500_000 || cfg.MonthlyMicroUSD != 100_250_000 {
		t.Fatalf("set = %+v, %v", cfg, err)
	}
	if !cfg.Enabled() || cfg.MeteredLimitMicroUSD != 150_000_000 {
		t.Fatalf("all four limits set = %+v, want enabled", cfg)
	}
	// Part of the set is as good as unset: the feature stays off.
	cfg, err = ParseAgentRunBudgetConfig(env(map[string]string{"MULTICA_AGENT_RUN_BUDGET_DAILY_USD": "10"}))
	if err != nil || cfg.Enabled() {
		t.Fatalf("partial = %+v, %v; want disabled", cfg, err)
	}
	for name, v := range map[string]string{"text": "abc", "negative": "-1", "zero": "0", "nan": "NaN", "inf": "Inf", "too fine": "0.0000001"} {
		_, err := ParseAgentRunBudgetConfig(env(map[string]string{"MULTICA_AGENT_RUN_BUDGET_DAILY_USD": v}))
		if err == nil {
			t.Fatalf("%s (%q) was accepted", name, v)
		}
	}
}
