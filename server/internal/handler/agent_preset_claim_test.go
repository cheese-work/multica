package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// snapshot is the run record of one claim generation (CHE-2247).
type snapshot struct {
	PresetName, PresetSource, Model, Thinking, Tier *string
	Skips, ContextArgs                              []byte
}

func readSnapshot(t *testing.T, taskID string) snapshot {
	t.Helper()
	var s snapshot
	dbfx.QueryRow(t, `
		SELECT preset_name, preset_source, preset_skips, claimed_model,
		       claimed_thinking_level, claimed_service_tier, claimed_context_args
		FROM agent_task_queue WHERE id = $1`, taskID).
		Scan(&s.PresetName, &s.PresetSource, &s.Skips, &s.Model, &s.Thinking, &s.Tier, &s.ContextArgs)
	return s
}

func (s snapshot) isEmpty() bool {
	return s.PresetName == nil && s.PresetSource == nil && s.Skips == nil && s.Model == nil &&
		s.Thinking == nil && s.Tier == nil && s.ContextArgs == nil
}

func str(p *string) string {
	if p == nil {
		return "<NULL>"
	}
	return *p
}

// presetAgent is a DM-ready agent on a runtime of the given provider with the
// given base settings.
func presetAgent(t *testing.T, provider, model, thinking string) (agentID, sessionID, runtimeID, daemonID string) {
	t.Helper()
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID = setupDirectChatSession(t, ctx, "preset claim")
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = $1 WHERE id = $2`, provider, runtimeID)
	dbfx.Exec(t, `UPDATE agent SET model = $1, thinking_level = $2 WHERE id = $3`, model, thinking, agentID)
	return
}

type presetSpec struct {
	model, thinking, tier *string
	ctxArgs               []string
	isDefault             bool
}

func sp(s string) *string { return &s }

func addPreset(t *testing.T, agentID, name string, p presetSpec) {
	t.Helper()
	params := db.CreateAgentPresetParams{
		WorkspaceID: parseUUID(testWorkspaceID), AgentID: parseUUID(agentID), Name: name, IsDefault: p.isDefault,
	}
	if p.model != nil {
		params.Model.String, params.Model.Valid = *p.model, true
	}
	if p.thinking != nil {
		params.ThinkingLevel.String, params.ThinkingLevel.Valid = *p.thinking, true
	}
	if p.tier != nil {
		params.ServiceTier.String, params.ServiceTier.Valid = *p.tier, true
	}
	if p.ctxArgs != nil {
		params.ContextArgs, _ = json.Marshal(p.ctxArgs)
	}
	if _, err := testHandler.Queries.CreateAgentPreset(context.Background(), params); err != nil {
		t.Fatalf("create preset %q: %v", name, err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_preset WHERE agent_id = $1`, agentID) })
}

func setDMPreset(t *testing.T, sessionID, name string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE chat_session SET preset_name = $1 WHERE id = $2`, name, sessionID)
}

// finishTask frees the chat session so the next message can be claimed.
func finishTask(t *testing.T, taskID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, taskID)
}

func skipsOf(t *testing.T, s snapshot) []map[string]string {
	t.Helper()
	var out []map[string]string
	if err := json.Unmarshal(s.Skips, &out); err != nil {
		t.Fatalf("preset_skips %q: %v", s.Skips, err)
	}
	return out
}

// R1: the claim applies the DM preset and records requested and claimed.
func TestClaimPreset_DMPresetRecordsRequestedAndClaimed(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "medium")
	addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5"), thinking: sp("low"), ctxArgs: []string{"--autocompact", "200k"}})
	setDMPreset(t, sessionID, "fast")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")

	claimed := claimForkTask(t, runtimeID, daemonID)

	if a := claimed.Agent; a.Model != "claude-sonnet-5-5" || a.ThinkingLevel != "low" {
		t.Fatalf("daemon payload model/thinking = %q/%q, want the preset's", a.Model, a.ThinkingLevel)
	}
	if !reflect.DeepEqual(claimed.Agent.CustomArgs, []string{"--autocompact", "200k"}) {
		t.Fatalf("daemon payload custom_args = %q", claimed.Agent.CustomArgs)
	}
	s := readSnapshot(t, claimed.ID)
	if str(s.PresetName) != "fast" || str(s.PresetSource) != "dm" || str(s.Model) != "claude-sonnet-5-5" ||
		str(s.Thinking) != "low" || string(s.ContextArgs) != `["--autocompact", "200k"]` || string(s.Skips) != "[]" {
		t.Fatalf("snapshot = %+v (skips %s, ctx %s)", s, s.Skips, s.ContextArgs)
	}
	if claimed.ClaimedModel == nil || *claimed.ClaimedModel != "claude-sonnet-5-5" {
		t.Fatalf("claim response claimed_model = %v", claimed.ClaimedModel)
	}
}

// With no preset anywhere the run is recorded as base, with the agent row.
func TestClaimPreset_NoPresetRecordsBase(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")

	s := readSnapshot(t, claimForkTask(t, runtimeID, daemonID).ID)
	if str(s.PresetName) != "base" || str(s.PresetSource) != "base" || str(s.Model) != "claude-opus-5-5" || str(s.Thinking) != "high" {
		t.Fatalf("snapshot = %+v", s)
	}
}

// A successful claim records the context args the daemon is handed, even when
// they are inherited from the agent's custom_args (base, or a preset that sets
// none). Known-empty is `[]`; NULL is reserved for a failed snapshot write.
func TestClaimPreset_SnapshotRecordsInheritedContextArgs(t *testing.T) {
	requireDB(t)
	cases := []struct {
		name       string
		customArgs string
		preset     *presetSpec
		want       string
	}{
		{"base", `["--autocompact","100k","--add-dir","/safe"]`, nil, `["--autocompact", "100k"]`},
		{"model-only preset", `["--autocompact","100k","--add-dir","/safe"]`, &presetSpec{model: sp("claude-sonnet-5-5")}, `["--autocompact", "100k"]`},
		{"preset replaces", `["--autocompact","100k","--add-dir","/safe"]`, &presetSpec{ctxArgs: []string{"--autocompact", "200k"}}, `["--autocompact", "200k"]`},
		{"equals form", `["--autocompact=100k"]`, nil, `["--autocompact=100k"]`},
		{"no context args", `["--add-dir","/safe"]`, nil, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
			dbfx.Exec(t, `UPDATE agent SET custom_args = $1::jsonb WHERE id = $2`, tc.customArgs, agentID)
			if tc.preset != nil {
				addPreset(t, agentID, "p", *tc.preset)
				setDMPreset(t, sessionID, "p")
			}

			s, _ := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

			if s.ContextArgs == nil || string(s.ContextArgs) != tc.want {
				t.Fatalf("claimed_context_args = %s, want %s", s.ContextArgs, tc.want)
			}
		})
	}
}

// I3 + I4: a preset changes only model, thinking level, service tier and
// context args. Everything else in the daemon payload is byte-equal, and the
// agent's sandbox flags keep their order.
func TestClaimPreset_OnlySettingsDifferFromBaseClaim(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "medium")
	dbfx.Exec(t, `UPDATE agent SET instructions = 'keep me', custom_env = '{"A":"1"}',
		custom_args = '["--sandbox","workspace-write","--autocompact","100k","--add-dir","/x"]' WHERE id = $1`, agentID)
	addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5"), thinking: sp("low"), ctxArgs: []string{"--autocompact", "200k"}})

	payload := func() map[string]any {
		var m map[string]any
		raw, _ := json.Marshal(claimForkTask(t, runtimeID, daemonID).Agent)
		_ = json.Unmarshal(raw, &m)
		return m
	}
	first := sendDirectChat(t, ctx, agentID, sessionID, "one")
	base := payload()
	finishTask(t, first)
	setDMPreset(t, sessionID, "fast")
	sendDirectChat(t, ctx, agentID, sessionID, "two")
	withPreset := payload()

	if got := withPreset["custom_args"]; !reflect.DeepEqual(got, []any{"--sandbox", "workspace-write", "--add-dir", "/x", "--autocompact", "200k"}) {
		t.Fatalf("custom_args = %v: sandbox flags must survive in order, only the context arg is replaced", got)
	}
	for _, k := range []string{"model", "thinking_level", "service_tier", "custom_args"} {
		delete(base, k)
		delete(withPreset, k)
	}
	if !reflect.DeepEqual(base, withPreset) {
		t.Fatalf("payload outside the four settings differs:\nbase   %v\npreset %v", base, withPreset)
	}
}

// I5: the task keeps its agent under any preset, and the vote gate reads the
// preset's model as the task's lab.
func TestClaimPreset_SameAgentAndPresetModelLabForVoteGate(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_VOTE_GATE_LABS", `{"claude/claude-opus-5-5":"anthropic","claude/claude-sonnet-5-5":"anthropic"}`)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5")})
	setDMPreset(t, sessionID, "fast")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")

	claimed := claimForkTask(t, runtimeID, daemonID)

	var taskAgent string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, claimed.ID).Scan(&taskAgent)
	if taskAgent != agentID || claimed.AgentID != agentID || claimed.Agent.ID != agentID {
		t.Fatalf("agent identity changed under a preset: task %s, response %s/%s, want %s", taskAgent, claimed.AgentID, claimed.Agent.ID, agentID)
	}
	if lab := voteLab("claude", str(readSnapshot(t, claimed.ID).Model)); lab != "anthropic" {
		t.Fatalf("vote lab of the claimed model = %q, want anthropic", lab)
	}
}

// An issue label preset:<name> wins over the agent default.
func TestClaimPreset_IssueLabelBeatsAgentDefault(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Preset label runtime")
	daemonID := "preset-label-daemon"
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'claude', daemon_id = $2 WHERE id = $1`, runtimeID, daemonID)
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Preset label agent")
	dbfx.Exec(t, `UPDATE agent SET model = 'claude-opus-5-5' WHERE id = $1`, agentID)
	addPreset(t, agentID, "dflt", presetSpec{thinking: sp("low"), isDefault: true})
	addPreset(t, agentID, "fast", presetSpec{thinking: sp("max")})
	var labelID string
	dbfx.QueryRow(t, `INSERT INTO issue_label (workspace_id, name, color) VALUES ($1, 'preset:fast', '#000') RETURNING id`, testWorkspaceID).Scan(&labelID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue_label WHERE id = $1`, labelID) })
	dbfx.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, issueID, labelID)
	taskID := seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)

	claimForkTask(t, runtimeID, daemonID)

	s := readSnapshot(t, taskID)
	if str(s.PresetName) != "fast" || str(s.PresetSource) != "issue_label" || str(s.Thinking) != "max" {
		t.Fatalf("snapshot = %+v", s)
	}
}

// staleCase claims after mutate() and expects the base row with a skip.
func claimAfter(t *testing.T, agentID, sessionID, runtimeID, daemonID string) (snapshot, AgentTaskResponse) {
	t.Helper()
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")
	claimed := claimForkTask(t, runtimeID, daemonID)
	return readSnapshot(t, claimed.ID), claimed
}

func wantSkip(t *testing.T, s snapshot, level, name, reason string) {
	t.Helper()
	want := map[string]string{"level": level, "name": name, "reason": reason}
	for _, got := range skipsOf(t, s) {
		if reflect.DeepEqual(got, want) {
			return
		}
	}
	t.Fatalf("preset_skips = %s, want to contain %v", s.Skips, want)
}

// E1: a Claude --autocompact preset goes stale when the agent is rebound to Codex.
func TestClaimPreset_E1_RebindMakesContextArgsStale(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	addPreset(t, agentID, "big", presetSpec{ctxArgs: []string{"--autocompact", "200k"}, isDefault: true})
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'codex' WHERE id = $1`, runtimeID)

	s, claimed := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

	if str(s.PresetName) != "base" || str(s.PresetSource) != "base" || len(claimed.Agent.CustomArgs) != 0 {
		t.Fatalf("snapshot = %+v, custom_args %q: a stale preset must fall back to base", s, claimed.Agent.CustomArgs)
	}
	wantSkip(t, s, "agent_default", "big", "context_args")
}

// E2: a preset pinned to an old-lab model goes stale when the base moves to another lab.
func TestClaimPreset_E2_PinnedModelLabLock(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_VOTE_GATE_LABS", `{"claude/claude-x0":"X","claude/claude-x1":"X","claude/claude-y":"Y"}`)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-x0", "high")
	addPreset(t, agentID, "pin", presetSpec{model: sp("claude-x1"), isDefault: true})
	dbfx.Exec(t, `UPDATE agent SET model = 'claude-y' WHERE id = $1`, agentID)

	s, _ := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

	if str(s.PresetSource) != "base" || str(s.Model) != "claude-y" {
		t.Fatalf("snapshot = %+v", s)
	}
	wantSkip(t, s, "agent_default", "pin", "lab_lock")
}

// E3: a preset that inherits the model stays valid through same-lab and cross-lab base edits.
func TestClaimPreset_E3_InheritedModelFollowsBase(t *testing.T) {
	requireDB(t)
	t.Setenv("MULTICA_VOTE_GATE_LABS", `{"claude/claude-x0":"X","claude/claude-x1":"X","claude/claude-y":"Y"}`)
	for name, newBase := range map[string]string{"same lab": "claude-x1", "other lab": "claude-y"} {
		t.Run(name, func(t *testing.T) {
			agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-x0", "high")
			addPreset(t, agentID, "think", presetSpec{thinking: sp("max"), isDefault: true})
			dbfx.Exec(t, `UPDATE agent SET model = $1 WHERE id = $2`, newBase, agentID)

			s, _ := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

			if str(s.PresetName) != "think" || str(s.PresetSource) != "agent_default" || str(s.Model) != newBase || str(s.Thinking) != "max" {
				t.Fatalf("snapshot = %+v", s)
			}
		})
	}
}

// E6: a tier-only preset goes stale on its own when a Codex agent is rebound to Claude.
func TestClaimPreset_E6_TierOnlyPresetStaleAfterRebind(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "codex", "gpt-6.1-sol", "high")
	addPreset(t, agentID, "turbo", presetSpec{tier: sp("fast"), isDefault: true})
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'claude' WHERE id = $1`, runtimeID)

	s, _ := claimAfter(t, agentID, sessionID, runtimeID, daemonID)

	if str(s.PresetSource) != "base" {
		t.Fatalf("snapshot = %+v", s)
	}
	wantSkip(t, s, "agent_default", "turbo", "service_tier")
}

// failSnapshotDB fails every snapshot write and passes the rest through.
type failSnapshotDB struct{ db.DBTX }

func (d failSnapshotDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "-- name: SetTaskClaimSnapshot") {
		return pgconn.CommandTag{}, errors.New("injected snapshot failure")
	}
	return d.DBTX.Exec(ctx, sql, args...)
}

// claimWithFailingSnapshot claims through a handler whose snapshot write fails.
func claimWithFailingSnapshot(t *testing.T, runtimeID, daemonID string) AgentTaskResponse {
	t.Helper()
	h := *testHandler
	h.Queries = db.New(failSnapshotDB{testPool})
	request := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID)
	request = withURLParam(request, "runtimeId", runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, h.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil {
		t.Fatal("no task was claimed")
	}
	return *response.Task
}

// R2: a failed snapshot write leaves the record NULL and the claim still succeeds.
func TestClaimPreset_R2_SnapshotFailureLeavesNullAndClaims(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")

	claimed := claimWithFailingSnapshot(t, runtimeID, daemonID)

	if s := readSnapshot(t, claimed.ID); !s.isEmpty() {
		t.Fatalf("snapshot = %+v, want all NULL", s)
	}
	if claimed.ClaimedModel != nil {
		t.Fatalf("claim response claimed_model = %v, want null when unrecorded", *claimed.ClaimedModel)
	}
}

// agedClaim claims once with a preset, then ages the dispatch so the next
// claim reclaims it as a new generation. It returns task id and the old generation.
func agedClaim(t *testing.T, withPreset bool) (taskID, runtimeID, daemonID, agentID, oldGeneration string) {
	t.Helper()
	var sessionID string
	agentID, sessionID, runtimeID, daemonID = presetAgent(t, "claude", "claude-opus-5-5", "high")
	if withPreset {
		addPreset(t, agentID, "fast", presetSpec{model: sp("claude-sonnet-5-5")})
		setDMPreset(t, sessionID, "fast")
	}
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")
	taskID = claimForkTask(t, runtimeID, daemonID).ID
	dbfx.QueryRow(t, `SELECT dispatched_at::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&oldGeneration)
	dbfx.Exec(t, `UPDATE agent_task_queue SET dispatched_at = now() - interval '1 hour', prepare_lease_expires_at = now() - interval '1 minute' WHERE id = $1`, taskID)
	dbfx.QueryRow(t, `SELECT dispatched_at::text FROM agent_task_queue WHERE id = $1`, taskID).Scan(&oldGeneration)
	return
}

// G1: claim B (a reclaim) whose snapshot write fails must not inherit claim A's record.
func TestClaimPreset_G1_ReclaimThenSnapshotFailureIsNull(t *testing.T) {
	requireDB(t)
	taskID, runtimeID, daemonID, _, _ := agedClaim(t, true)
	if s := readSnapshot(t, taskID); str(s.PresetName) != "fast" {
		t.Fatalf("claim A snapshot = %+v", s)
	}

	reclaimed := claimWithFailingSnapshot(t, runtimeID, daemonID)

	if reclaimed.ID != taskID {
		t.Fatalf("reclaimed task %s, want %s", reclaimed.ID, taskID)
	}
	if s := readSnapshot(t, taskID); !s.isEmpty() {
		t.Fatalf("claim B snapshot = %+v, want all NULL (claim A's values must not survive)", s)
	}
}

// G2: a snapshot write that carries claim A's generation is refused after claim B.
func TestClaimPreset_G2_OldGenerationWriteIsRefused(t *testing.T) {
	requireDB(t)
	taskID, runtimeID, daemonID, _, _ := agedClaim(t, true)
	var oldAt db.AgentTaskQueue
	oldAt, _ = testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	reclaimed := claimForkTask(t, runtimeID, daemonID)
	if reclaimed.ID != taskID {
		t.Fatalf("reclaimed task %s, want %s", reclaimed.ID, taskID)
	}
	before := readSnapshot(t, taskID)

	rows, err := testHandler.Queries.SetTaskClaimSnapshot(context.Background(), db.SetTaskClaimSnapshotParams{
		ID: parseUUID(taskID), RuntimeID: oldAt.RuntimeID, DispatchedAt: oldAt.DispatchedAt,
		PresetName: pgtype.Text{String: "stale", Valid: true}, ClaimedModel: pgtype.Text{String: "stale", Valid: true},
	})
	if err != nil || rows != 0 {
		t.Fatalf("old-generation write: rows=%d err=%v, want 0 rows", rows, err)
	}
	if after := readSnapshot(t, taskID); !reflect.DeepEqual(before, after) {
		t.Fatalf("claim B snapshot changed: %+v -> %+v", before, after)
	}
}

// G3: returning a claim to the queue clears its snapshot.
func TestClaimPreset_G3_RequeueClearsSnapshot(t *testing.T) {
	requireDB(t)
	agentID, sessionID, runtimeID, daemonID := presetAgent(t, "claude", "claude-opus-5-5", "high")
	sendDirectChat(t, context.Background(), agentID, sessionID, "hello")
	claimed := claimForkTask(t, runtimeID, daemonID)
	task, _ := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(claimed.ID))
	if s := readSnapshot(t, claimed.ID); s.isEmpty() {
		t.Fatal("claim wrote no snapshot")
	}

	if _, err := testHandler.Queries.RequeueAgentTaskAfterClaimFailure(context.Background(), db.RequeueAgentTaskAfterClaimFailureParams{
		TaskID: task.ID, RuntimeID: task.RuntimeID, DispatchedAt: task.DispatchedAt,
	}); err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if s := readSnapshot(t, claimed.ID); !s.isEmpty() {
		t.Fatalf("snapshot after requeue = %+v, want all NULL", s)
	}
}

// G4: every statement that writes dispatched_at also resets every snapshot
// column, so a future generation writer cannot skip the reset.
func TestClaimPreset_G4_EveryDispatchedAtWriterResetsSnapshot(t *testing.T) {
	columns := []string{"preset_name", "preset_source", "preset_skips", "claimed_model",
		"claimed_thinking_level", "claimed_service_tier", "claimed_context_args"}
	assign := regexp.MustCompile(`(?m)^\s*(SET\s+)?dispatched_at\s*=`)
	entries, err := os.ReadDir("../../pkg/db/queries")
	if err != nil {
		t.Fatal(err)
	}
	writers := 0
	for _, e := range entries {
		src, err := os.ReadFile("../../pkg/db/queries/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(string(src), "-- name: ")[1:] {
			if !assign.MatchString(stmt) {
				continue
			}
			writers++
			name, _, _ := strings.Cut(stmt, " ")
			for _, c := range columns {
				if !regexp.MustCompile(`\b` + c + `\s*=\s*NULL`).MatchString(stmt) {
					t.Errorf("%s writes dispatched_at without resetting %s", name, c)
				}
			}
		}
	}
	if writers < 4 {
		t.Fatalf("found %d dispatched_at writers, want at least the 4 known (claim, requeue, 2 reclaims)", writers)
	}
}
