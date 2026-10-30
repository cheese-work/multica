package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestClaimTaskByRuntime_LeaderSessionCompaction: a leader run whose resumed
// session already holds more than the token threshold starts a fresh session
// instead (the cold issue/comment read rebuilds its state); every other run
// resumes unchanged.
func TestClaimTaskByRuntime_LeaderSessionCompaction(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name        string
		env         string
		priorTokens any // nil = the prior run reported no context size
		leader      bool
		wantResumed bool
	}{
		{name: "leader over default threshold starts fresh", priorTokens: 120_000, leader: true, wantResumed: false},
		{name: "leader at threshold starts fresh", priorTokens: leaderSessionMaxTokensDefault, leader: true, wantResumed: false},
		{name: "leader under threshold resumes", priorTokens: 79_999, leader: true, wantResumed: true},
		{name: "leader with unknown context size resumes", priorTokens: nil, leader: true, wantResumed: true},
		{name: "non-leader over threshold resumes", priorTokens: 120_000, leader: false, wantResumed: true},
		{name: "env raises the threshold", env: "200000", priorTokens: 120_000, leader: true, wantResumed: true},
		{name: "env lowers the threshold", env: "50000", priorTokens: 60_000, leader: true, wantResumed: false},
		{name: "invalid env falls back to default", env: "lots", priorTokens: 120_000, leader: true, wantResumed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.env != "" {
				t.Setenv(leaderSessionMaxTokensEnv, tc.env)
			}
			fx := newSquadBriefingClaimFixture(t, ctx, "Leader compaction")
			dbfx.Task(t, fx.AgentID, testutil.Cols{
				"runtime_id":     fx.RuntimeID,
				"issue_id":       fx.IssueID,
				"status":         "completed",
				"session_id":     "leader-compaction-session",
				"work_dir":       "/tmp/leader-compaction-workdir",
				"context_tokens": tc.priorTokens,
				"context_window": 200_000,
				"started_at":     testutil.Raw("now() - interval '1 hour'"),
				"completed_at":   testutil.Raw("now() - interval '50 minutes'"),
			})
			enqueueClaimTask(t, ctx, fx, tc.leader, tc.leader)

			resp := claimCommentTask(t, fx.RuntimeID, "leader-compaction-claim")
			if resumed := resp.Task.PriorSessionID == "leader-compaction-session"; resumed != tc.wantResumed {
				t.Fatalf("prior_session_id = %q, resumed = %v, want resumed = %v", resp.Task.PriorSessionID, resumed, tc.wantResumed)
			}
			if resp.Task.PriorWorkDir != "/tmp/leader-compaction-workdir" {
				t.Errorf("prior_work_dir = %q — a fresh session must keep the workdir", resp.Task.PriorWorkDir)
			}
		})
	}
}

// TestClaimTaskByRuntime_StaleClaudeSession: a Claude prior that ended at or
// past the prompt-cache TTL starts a fresh session in the same workdir (the
// resume would re-write the whole transcript at the cache-write price); a
// fresher Claude prior and every other provider resume unchanged.
func TestClaimTaskByRuntime_StaleClaudeSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name        string
		env         string
		provider    string
		idle        string // postgres interval since the prior run ended
		wantResumed bool
	}{
		{name: "claude idle past threshold starts fresh", provider: "claude", idle: "56 minutes", wantResumed: false},
		{name: "claude idle just under threshold resumes", provider: "claude", idle: "50 minutes", wantResumed: true},
		{name: "other provider idle past threshold resumes", provider: "codex", idle: "3 hours", wantResumed: true},
		{name: "env raises the threshold", provider: "claude", env: "120", idle: "90 minutes", wantResumed: true},
		{name: "env lowers the threshold", provider: "claude", env: "10", idle: "15 minutes", wantResumed: false},
		{name: "invalid env falls back to default", provider: "claude", env: "soon", idle: "56 minutes", wantResumed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.env != "" {
				t.Setenv(staleSessionMinutesEnv, tc.env)
			}
			fx := newSquadBriefingClaimFixture(t, ctx, "Stale session")
			testPool.Exec(ctx, `UPDATE agent_runtime SET provider = $1 WHERE id = $2`, tc.provider, fx.RuntimeID)
			dbfx.Task(t, fx.AgentID, testutil.Cols{
				"runtime_id":   fx.RuntimeID,
				"issue_id":     fx.IssueID,
				"status":       "completed",
				"session_id":   "stale-session",
				"work_dir":     "/tmp/stale-session-workdir",
				"started_at":   testutil.Raw("now() - interval '" + tc.idle + "' - interval '5 minutes'"),
				"completed_at": testutil.Raw("now() - interval '" + tc.idle + "'"),
			})
			enqueueClaimTask(t, ctx, fx, false, false)

			resp := claimCommentTask(t, fx.RuntimeID, "stale-session-claim")
			if resumed := resp.Task.PriorSessionID == "stale-session"; resumed != tc.wantResumed {
				t.Fatalf("prior_session_id = %q, resumed = %v, want resumed = %v", resp.Task.PriorSessionID, resumed, tc.wantResumed)
			}
			if resp.Task.PriorWorkDir != "/tmp/stale-session-workdir" {
				t.Errorf("prior_work_dir = %q — a fresh session must keep the workdir", resp.Task.PriorWorkDir)
			}
		})
	}
}
