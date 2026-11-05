package handler

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/dispatch"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AgentRunBudgetConfig caps what agents may add to issue run budgets (CHE-3251).
// Money is whole micro-USD so no cap is ever compared as a float. The feature
// is off until all four are set.
//
// The cap is admission control with a margin, not a hard cap: it limits the
// credit agents grant, at the configured USD per run, and a run can cost more
// than that.
type AgentRunBudgetConfig struct {
	PerRunMicroUSD       int64 // price of one granted run
	DailyMicroUSD        int64 // agent-added credit per ICT day
	MonthlyMicroUSD      int64 // agent-added credit per ICT month, used or not
	MeteredLimitMicroUSD int64 // monthly metered-API limit; checked by the admission gate (CHE-3251 PR B)

	// meteredGateReady is true only once the metered-limit checks (CHE-3251
	// PR B) exist. Nothing sets it in production code yet, so agent grants stay
	// refused with reason "disabled" even when all four limits are set: this
	// layer enforces the daily and monthly caps but not the metered limit, and
	// enabling it without that check would grant credit the metered limit
	// never bounds. PR B sets it where it wires the checks in.
	meteredGateReady bool
}

func (c AgentRunBudgetConfig) Enabled() bool {
	return c.meteredGateReady && c.PerRunMicroUSD > 0 && c.DailyMicroUSD > 0 && c.MonthlyMicroUSD > 0 && c.MeteredLimitMicroUSD > 0
}

var agentRunBudgetEnv = []struct {
	name string
	set  func(*AgentRunBudgetConfig, int64)
}{
	{"MULTICA_AGENT_RUN_BUDGET_USD_PER_RUN", func(c *AgentRunBudgetConfig, v int64) { c.PerRunMicroUSD = v }},
	{"MULTICA_AGENT_RUN_BUDGET_DAILY_USD", func(c *AgentRunBudgetConfig, v int64) { c.DailyMicroUSD = v }},
	{"MULTICA_AGENT_RUN_BUDGET_MONTHLY_USD", func(c *AgentRunBudgetConfig, v int64) { c.MonthlyMicroUSD = v }},
	{"MULTICA_AGENT_RUN_BUDGET_METERED_LIMIT_USD", func(c *AgentRunBudgetConfig, v int64) { c.MeteredLimitMicroUSD = v }},
}

// ParseAgentRunBudgetConfig reads the four MULTICA_AGENT_RUN_BUDGET_* values.
// An unset value leaves the feature off; a set value that is not a positive USD
// amount of at most six decimals is an error, so a typo stops the server
// instead of silently turning a cap off.
func ParseAgentRunBudgetConfig(getenv func(string) string) (AgentRunBudgetConfig, error) {
	var cfg AgentRunBudgetConfig
	for _, e := range agentRunBudgetEnv {
		raw := strings.TrimSpace(getenv(e.name))
		if raw == "" {
			continue
		}
		usd, err := strconv.ParseFloat(raw, 64)
		micro := math.Round(usd * 1e6)
		if err != nil || math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 || usd > 1e9 || math.Abs(usd*1e6-micro) > 1e-3 {
			return AgentRunBudgetConfig{}, fmt.Errorf("%s=%q: want a positive USD amount with at most 6 decimals", e.name, raw)
		}
		e.set(&cfg, int64(micro))
	}
	return cfg, nil
}

// The agent cap counts in ICT days and months.
var ict = time.FixedZone("ICT", 7*3600)

func ictDayStart(t time.Time) time.Time {
	y, m, d := t.In(ict).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, ict)
}

func ictNextDay(t time.Time) time.Time {
	y, m, d := t.In(ict).Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, ict)
}

func ictMonthStart(t time.Time) time.Time {
	y, m, _ := t.In(ict).Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, ict)
}

func ictNextMonth(t time.Time) time.Time {
	y, m, _ := t.In(ict).Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, ict)
}

func usd(micro int64) float64 { return float64(micro) / 1e6 }

// admitAgentRunBudgetCaller decides whether an agent request may raise or top
// up issue's run budget, and writes the refusal when not. Everything fails
// closed: only the live task token of an agent other than the issue's assignee,
// in the issue's workspace, outside a budget recovery run.
func (h *Handler) admitAgentRunBudgetCaller(w http.ResponseWriter, r *http.Request, issue db.Issue, agentID string) bool {
	refuse := func(msg string) bool {
		writeError(w, http.StatusForbidden, msg)
		return false
	}
	// resolveActor also calls an agent headers-only request an agent; only the
	// task token Auth stamped counts here.
	if r.Header.Get("X-Actor-Source") != "task_token" || dispatch.BudgetRecoveryCaller(r.Context()) {
		return refuse("only a person can change an issue's run budget")
	}
	if !h.AgentRunBudget.Enabled() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "agents cannot raise run budgets on this server", "reason": "disabled"})
		return false
	}
	agentUUID, err := util.ParseUUID(agentID)
	taskUUID, taskErr := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil || taskErr != nil {
		return refuse("only a person can change an issue's run budget")
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskUUID)
	if err != nil || task.AgentID != agentUUID || isTerminalTaskStatus(task.Status) {
		return refuse("the task behind this request is not running")
	}
	if issue.AssigneeType.Valid && issue.AssigneeType.String == "agent" && issue.AssigneeID == agentUUID {
		return refuse("an agent cannot raise its own issue's run budget")
	}
	agent, err := h.Queries.GetAgent(r.Context(), agentUUID)
	if err != nil || agent.WorkspaceID != issue.WorkspaceID {
		return refuse("the agent is not in this issue's workspace")
	}
	return true
}

// checkAgentRunBudgetCap refuses an agent's grant of runs when it would pass the
// daily or the monthly cap. It runs inside the grant's transaction, after the
// issue's lock, and takes the instance-wide cap lock: the sums it reads cannot
// change until that transaction ends. It writes the refusal and reports false
// when the grant must not happen.
func (h *Handler) checkAgentRunBudgetCap(w http.ResponseWriter, r *http.Request, q *db.Queries, runs int) (ok bool, err error) {
	if err := q.LockAgentRunBudgetCap(r.Context()); err != nil {
		return false, err
	}
	cfg, now := h.AgentRunBudget, time.Now()
	for _, c := range []struct {
		reason string
		limit  int64
		since  time.Time
		resets time.Time
	}{
		{"daily", cfg.DailyMicroUSD, ictDayStart(now), ictNextDay(now)},
		{"monthly_share", cfg.MonthlyMicroUSD, ictMonthStart(now), ictNextMonth(now)},
	} {
		added, err := q.SumAgentAddedRuns(r.Context(), pgtype.Timestamptz{Time: c.since, Valid: true})
		if err != nil {
			return false, err
		}
		used := added * cfg.PerRunMicroUSD
		if used+int64(runs)*cfg.PerRunMicroUSD <= c.limit {
			continue
		}
		remaining := max(c.limit-used, 0)
		retry := max(int(time.Until(c.resets).Seconds()), 1)
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "agent run-budget cap reached", "reason": c.reason,
			"limit_usd": usd(c.limit), "used_usd": usd(used), "remaining_usd": usd(remaining),
			"remaining_runs": remaining / cfg.PerRunMicroUSD, "requested_runs": runs,
			"resets_at": c.resets.Format(time.RFC3339),
		})
		return false, nil
	}
	return true, nil
}
