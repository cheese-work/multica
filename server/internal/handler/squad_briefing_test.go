package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestSquadOperatingProtocolScopesParentStatusOwnership is the guard for the
// MUL-5156 review finding: the briefing is injected on every leader path,
// including an @squad mention on an issue assigned to someone else. Status
// ownership must not ride along — a guest leader gets an explicit prohibition
// instead of the grant, so the model never has to infer the boundary.
func TestSquadOperatingProtocolScopesParentStatusOwnership(t *testing.T) {
	guest := squadOperatingProtocolFor(false)
	compactGuest := strings.Join(strings.Fields(guest), " ")

	for _, want := range []string{
		"Do NOT change this issue's status",
		"not assigned to your squad",
		"never run `multica issue status` on it",
	} {
		if !strings.Contains(compactGuest, want) {
			t.Errorf("expected guest-leader protocol to contain %q\n--- protocol ---\n%s", want, guest)
		}
	}
	// The grant must be entirely absent — not merely qualified.
	for _, forbidden := range []string{
		"Own the parent issue status",
		"multica issue status <issue-id> in_review",
	} {
		if strings.Contains(compactGuest, forbidden) {
			t.Errorf("guest-leader protocol must not contain status grant %q\n--- protocol ---\n%s", forbidden, guest)
		}
	}

	// Everything that is not the status responsibility is identical, so a
	// guest leader still coordinates, delegates, and records activity.
	owner := squadOperatingProtocolFor(true)
	for _, shared := range []string{
		"## Squad Operating Protocol",
		"Delegate by @mention",
		"Record your evaluation",
		"Stop after dispatching",
		"Never both for the same work.",
	} {
		if !strings.Contains(owner, shared) || !strings.Contains(guest, shared) {
			t.Errorf("expected %q in both protocol variants", shared)
		}
	}

	// Both variants must keep the protocol header. The daemon no longer
	// derives IsSquadLeader from it (MUL-5811 — it reads is_leader_task /
	// squad_id off the claim), but it is still the section title the leader
	// rules in the brief and the per-turn prompt refer to by name.
	if !strings.Contains(guest, "## Squad Operating Protocol") {
		t.Error("guest-leader protocol lost its section header")
	}
}

// seedSquadForBriefing creates a squad with the seeded test agent as
// leader. Returns the loaded db.Squad and a cleanup-registered ID.
func seedSquadForBriefing(t *testing.T, leaderID string, name, instructions string) db.Squad {
	t.Helper()
	ctx := context.Background()

	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id, instructions)
		VALUES ($1, $2, '', $3, $4, $5)
		RETURNING id
	`, testWorkspaceID, name, leaderID, testUserID, instructions).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, squadID)
	})

	uuid := util.MustParseUUID(squadID)
	squad, err := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          uuid,
		WorkspaceID: util.MustParseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load squad: %v", err)
	}
	return squad
}

func addAgentMember(t *testing.T, squadID pgtype.UUID, agentID, role string) {
	t.Helper()
	if _, err := testHandler.Queries.AddSquadMember(context.Background(), db.AddSquadMemberParams{
		SquadID:    squadID,
		MemberType: "agent",
		MemberID:   util.MustParseUUID(agentID),
		Role:       role,
	}); err != nil {
		t.Fatalf("add agent member: %v", err)
	}
}

func addHumanMember(t *testing.T, squadID pgtype.UUID, userID, role string) {
	t.Helper()
	if _, err := testHandler.Queries.AddSquadMember(context.Background(), db.AddSquadMemberParams{
		SquadID:    squadID,
		MemberType: "member",
		MemberID:   util.MustParseUUID(userID),
		Role:       role,
	}); err != nil {
		t.Fatalf("add human member: %v", err)
	}
}

// seededLeaderAgent loads the first seeded agent in the test workspace.
func seededLeaderAgent(t *testing.T) (id, name string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT id, name FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&id, &name); err != nil {
		t.Fatalf("load seeded agent: %v", err)
	}
	return id, name
}

// seededHumanMember returns the (member_row_id, user_id, user_name) of the
// test fixture's human member in the workspace.
func seededHumanMember(t *testing.T) (memberID, userID, userName string) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(), `
		SELECT m.id, u.id, u.name
		FROM member m JOIN "user" u ON u.id = m.user_id
		WHERE m.workspace_id = $1 ORDER BY m.created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&memberID, &userID, &userName); err != nil {
		t.Fatalf("load seeded member: %v", err)
	}
	return
}

// TestSquadOperatingProtocolOwnsNoActionRule pins the protocol as the single
// statement of the no_action rule (MUL-6984). It used to be written four
// times — here, in the per-turn prompt, in the brief's workflow step 4, and in
// the brief's ## Output — and the four copies had already drifted: only some
// of them carried the MUL-6622 / GH #7487 escape hatch, which is what keeps a
// FAILED `squad activity` call from ending the turn in silence. The other
// three surfaces now point here, so this text has to carry the whole rule:
// the prohibition, its exact scope, and the failure fallback.
func TestSquadOperatingProtocolOwnsNoActionRule(t *testing.T) {
	for _, ownsParentStatus := range []bool{true, false} {
		protocol := squadOperatingProtocolFor(ownsParentStatus)
		compact := strings.Join(strings.Fields(protocol), " ")

		for _, want := range []string{
			// the rule and how it is recorded
			"multica squad activity <issue-id> <outcome> --reason",
			"record `no_action` and exit silently",
			// what "silently" forbids — MUL-2168 was a leader posting
			// "no reply needed. Exiting silently."
			"posting NO comment at all",
			"not one saying you are exiting",
			// MUL-6622 / #7487: the prohibition lapses when the call fails,
			// because the server only rejects a leader comment once the
			// no_action activity exists.
			"holds only while the call succeeds",
			"responsibility 3 applies",
			// one comment, not two — the fallback must not collide with the
			// one-comment-per-turn rule
			"never post a second comment",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}
	}
}

// TestSquadOperatingProtocolRequiresReconciliationBeforeNoAction covers the
// CHE-329/CHE-346 workspace Completion Contract's verification-cases table.
// A first substantive merge/acceptance/blocker-resolution event on an issue
// must not be silently no_action'd without the reconciliation steps; the
// legitimate quiet-no_action paths (routine progress update, duplicate /
// already-reconciled notification) must still work.
func TestSquadOperatingProtocolRequiresReconciliationBeforeNoAction(t *testing.T) {
	for _, ownsParentStatus := range []bool{true, false} {
		protocol := squadOperatingProtocolFor(ownsParentStatus)
		compact := strings.Join(strings.Fields(protocol), " ")

		// Case A: a first substantive merge/acceptance/blocker-resolution
		// event requires the reconciliation steps and a published result
		// before no_action is legal.
		for _, want := range []string{
			"substantive merge, acceptance, or",
			"blocker-resolution event",
			"FIRST time this issue sees",
			"read the current comment history",
			"read candidate/check",
			"read the issue's current status",
			"remaining owner/action exists",
			"publish exactly one result",
			"comment and/or a status change",
			"protocol violation, not a shortcut",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing reconciliation requirement %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}

		// Case A2 (Terra P1 correction, CHE-348/PR #11 review of cb28d12c):
		// the published result must explicitly state the remaining
		// owner/action, or explicitly state none remains — a status-only
		// change does not satisfy this on its own.
		for _, want := range []string{
			"must explicitly",
			"state the remaining owner/action, or explicitly state that none",
			"remains",
			"A status change alone does not satisfy this",
			"the explicit owner/action statement belongs in a comment even on turns that also change status",
			"publishing a result that omits the owner/action statement, is a",
			"protocol violation, not a shortcut",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing mandatory owner/action statement requirement %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}

		// Case B: the legitimate quiet no_action path (routine progress
		// update, or a duplicate/already-reconciled notification) must not
		// regress — this is the case the original MUL-6984 rule protects.
		for _, want := range []string{
			"routine progress update that requires no response",
			"duplicate /",
			"already-actioned notification of an event this issue already",
			"reconciled",
			"record `no_action` and exit",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing legitimate quiet no_action path %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}
	}
}

// TestSquadParentStatusOwnedAllowsDoneOrInReview covers Cases C and D of the
// CHE-329/CHE-346 Completion Contract: the owning leader must be able to
// choose `done` when no pending human action remains, and `in_review` when
// one does — not be forced into `in_review` unconditionally (contract item 4).
func TestSquadParentStatusOwnedAllowsDoneOrInReview(t *testing.T) {
	compact := strings.Join(strings.Fields(squadParentStatusOwned), " ")

	// Case C: no pending human action remains → `done` is permitted.
	for _, want := range []string{
		"No pending human action remains",
		"outcome verified, all gates satisfied",
		"multica issue status <issue-id> done",
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("expected squadParentStatusOwned to permit `done` via %q\n--- text ---\n%s", want, squadParentStatusOwned)
		}
	}

	// Case D: a concrete pending human action remains → `in_review` is
	// still required.
	for _, want := range []string{
		"A concrete pending human action remains",
		"multica issue status <issue-id> in_review",
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("expected squadParentStatusOwned to still require `in_review` via %q\n--- text ---\n%s", want, squadParentStatusOwned)
		}
	}

	// The old unconditional routing to in_review (leaving `done` to a human)
	// must be gone — that was the exact contradiction this fix removes.
	if strings.Contains(compact, "Leave `done` to a human reviewer") {
		t.Errorf("squadParentStatusOwned still unconditionally routes to in_review and defers `done` to a human:\n%s", squadParentStatusOwned)
	}
}

// TestSquadParentStatusNotOwnedForbidsAnyStatusWrite is Case E: the
// guest/mention path must still forbid ANY status write, unaffected by the
// done/in_review choice granted to owning leaders.
func TestSquadParentStatusNotOwnedForbidsAnyStatusWrite(t *testing.T) {
	compact := strings.Join(strings.Fields(squadParentStatusNotOwned), " ")
	for _, want := range []string{
		"Do NOT change this issue's status",
		"never run `multica issue status` on it",
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("expected squadParentStatusNotOwned to forbid status writes via %q\n--- text ---\n%s", want, squadParentStatusNotOwned)
		}
	}
	for _, forbidden := range []string{
		"multica issue status <issue-id> done",
		"multica issue status <issue-id> in_review",
	} {
		if strings.Contains(compact, forbidden) {
			t.Errorf("squadParentStatusNotOwned must not contain a runnable status command %q\n--- text ---\n%s", forbidden, squadParentStatusNotOwned)
		}
	}
}

func TestBuildSquadLeaderBriefing_FullSquad(t *testing.T) {
	ctx := context.Background()
	leaderID, leaderName := seededLeaderAgent(t)

	squad := seedSquadForBriefing(t, leaderID, "Full Squad", "Always write tests.")

	helper1 := createHandlerTestAgent(t, "Helper One", []byte("[]"))
	helper2 := createHandlerTestAgent(t, "Helper Two", []byte("[]"))
	addAgentMember(t, squad.ID, helper1, "implementer")
	addAgentMember(t, squad.ID, helper2, "")

	memberRowID, userID, userName := seededHumanMember(t)
	_ = memberRowID
	addHumanMember(t, squad.ID, userID, "reviewer")

	out := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true)

	for _, want := range []string{
		"## Squad Operating Protocol",
		"## Squad Roster",
		"Leader (you):",
		leaderName,
		"## Squad Instructions (Full Squad)",
		"Always write tests.",
		"`[@Helper One](mention://agent/" + helper1 + ")`",
		"`[@Helper Two](mention://agent/" + helper2 + ")`",
		`role: "implementer"`,
		`role: "reviewer"`,
		"`[@" + userName + "](mention://member/" + userID + ")`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected briefing to contain %q\n--- briefing ---\n%s", want, out)
		}
	}

	// Helper Two has no role — must NOT render an empty role: "" segment.
	if strings.Contains(out, `Helper Two — agent, role: ""`) {
		t.Errorf("expected empty role to be omitted, got: %s", out)
	}
}

// assignSkillToAgent creates a workspace skill and attaches it to the agent.
func assignSkillToAgent(t *testing.T, agentID, skillName string) {
	t.Helper()
	ctx := context.Background()
	var skillID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO skill (workspace_id, name, description, content, created_by)
		VALUES ($1, $2, '', '', $3)
		RETURNING id
	`, testWorkspaceID, skillName, testUserID).Scan(&skillID); err != nil {
		t.Fatalf("create skill %s: %v", skillName, err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, `DELETE FROM agent_skill WHERE agent_id = $1 AND skill_id = $2`, agentID, skillID); err != nil {
			t.Errorf("cleanup agent skill %s/%s: %v", agentID, skillName, err)
		}
		if _, err := testPool.Exec(ctx, `DELETE FROM skill WHERE id = $1`, skillID); err != nil {
			t.Errorf("cleanup skill %s: %v", skillName, err)
		}
	})
	if _, err := testPool.Exec(ctx,
		`INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`,
		agentID, skillID,
	); err != nil {
		t.Fatalf("assign skill %s to agent: %v", skillName, err)
	}
}

// TestBuildSquadLeaderBriefing_MemberSkillsInRoster locks in the delegation
// fix: an agent member's assigned skills appear in the leader roster so the
// leader can route by capability. Agents with no skills get an explicit
// marker; human members never carry a skills segment.
func TestBuildSquadLeaderBriefing_MemberSkillsInRoster(t *testing.T) {
	ctx := context.Background()
	leaderID, _ := seededLeaderAgent(t)
	squad := seedSquadForBriefing(t, leaderID, "Skilled Squad", "")

	skilled := createHandlerTestAgent(t, "Skilled Bot", []byte("[]"))
	addAgentMember(t, squad.ID, skilled, "backend")
	// ListAgentSkillNamesByAgentIDs orders by name ASC → "polars" before "stat…".
	assignSkillToAgent(t, skilled, "polars")
	assignSkillToAgent(t, skilled, "statistical-analysis")

	plain := createHandlerTestAgent(t, "Plain Bot", []byte("[]"))
	addAgentMember(t, squad.ID, plain, "")

	memberRowID, userID, userName := seededHumanMember(t)
	_ = memberRowID
	addHumanMember(t, squad.ID, userID, "reviewer")

	out := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true)

	if !strings.Contains(out, "skills: polars, statistical-analysis") {
		t.Errorf("expected skilled member skills in roster, got:\n%s", out)
	}
	if !strings.Contains(out, "Plain Bot — agent — no skills assigned") {
		t.Errorf("expected no-skills marker for skill-less agent, got:\n%s", out)
	}
	if strings.Contains(out, userName+" — member (human), role: \"reviewer\" — skills:") ||
		strings.Contains(out, userName+" — member (human), role: \"reviewer\" — no skills") {
		t.Errorf("human member must not render a skills segment, got:\n%s", out)
	}
}

func TestBuildSquadLeaderBriefing_OnlyLeader(t *testing.T) {
	ctx := context.Background()
	leaderID, _ := seededLeaderAgent(t)
	squad := seedSquadForBriefing(t, leaderID, "Solo Squad", "")

	out := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true)
	if !strings.Contains(out, "Members: (none — you are the only member of this squad)") {
		t.Errorf("expected lone-leader fallback line, got:\n%s", out)
	}
	// No user instructions → no Squad Instructions section.
	if strings.Contains(out, "## Squad Instructions") {
		t.Errorf("expected no Squad Instructions section when empty, got:\n%s", out)
	}
}

func TestBuildSquadLeaderBriefing_SkipsArchivedAgent(t *testing.T) {
	ctx := context.Background()
	leaderID, _ := seededLeaderAgent(t)
	squad := seedSquadForBriefing(t, leaderID, "Archive Squad", "")

	archived := createHandlerTestAgent(t, "Retired Bot", []byte("[]"))
	addAgentMember(t, squad.ID, archived, "")
	if _, err := testPool.Exec(ctx,
		`UPDATE agent SET archived_at = now(), archived_by = $1 WHERE id = $2`,
		testUserID, archived,
	); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	out := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true)
	if strings.Contains(out, "Retired Bot") {
		t.Errorf("archived agent should not appear in roster:\n%s", out)
	}
	if strings.Contains(out, archived) {
		t.Errorf("archived agent UUID should not appear in roster:\n%s", out)
	}
}

// TestBuildSquadLeaderBriefing_MentionsRoundTrip is the contract test
// guaranteeing every emitted mention markdown string parses back through
// util.ParseMentions to its (type, id). If this ever breaks, the leader's
// dispatch comments will silently fail to trigger anyone.
func TestBuildSquadLeaderBriefing_MentionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	leaderID, _ := seededLeaderAgent(t)
	squad := seedSquadForBriefing(t, leaderID, "Mention Round Trip", "")

	helper := createHandlerTestAgent(t, "Round Trip Bot", []byte("[]"))
	addAgentMember(t, squad.ID, helper, "")

	memberRowID, userID, _ := seededHumanMember(t)
	_ = memberRowID
	addHumanMember(t, squad.ID, userID, "")

	out := buildSquadLeaderBriefing(ctx, testHandler.Queries, squad, true)
	mentions := util.ParseMentions(out)

	wantIDs := map[string]string{
		leaderID: "agent",
		helper:   "agent",
		userID:   "member",
	}
	got := make(map[string]string, len(mentions))
	for _, m := range mentions {
		got[m.ID] = m.Type
	}
	for id, kind := range wantIDs {
		if got[id] != kind {
			t.Errorf("expected %s mention for id %s, got %q (all parsed: %#v)", kind, id, got[id], mentions)
		}
	}
}

// claimAndDecodeAgent runs ClaimTaskByRuntime for the given runtime and
// returns the agent block of the response. Fails the test on non-200.
func claimAndDecodeAgent(t *testing.T, runtimeID string) *TaskAgentData {
	t.Helper()
	w := httptest.NewRecorder()
	req := newDaemonTokenRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, "test-claim-squad-briefing")
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *struct {
			Agent *TaskAgentData `json:"agent"`
		} `json:"task"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Task == nil || resp.Task.Agent == nil {
		t.Fatalf("expected task.agent in response, got: %s", w.Body.String())
	}
	return resp.Task.Agent
}

// queueSquadIssueTaskFor creates an issue assigned to the squad and a queued
// task for the given (agentID, runtimeID). Returns the issue + task IDs.
func queueSquadIssueTaskFor(t *testing.T, squadID, agentID, runtimeID string, issueNumber int) (issueID, taskID string) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx, `
INSERT INTO issue (
workspace_id, title, status, priority, creator_id, creator_type,
assignee_type, assignee_id, number, position
) VALUES ($1, 'Squad briefing claim test', 'todo', 'medium', $2, 'member',
'squad', $3, $4, 0)
RETURNING id
`, testWorkspaceID, testUserID, squadID, issueNumber).Scan(&issueID); err != nil {
		t.Fatalf("create squad-assigned issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	if err := testPool.QueryRow(ctx, `
INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, is_leader_task, squad_id)
VALUES ($1, $2, $3, 'queued', 0,
        ($1::uuid = (SELECT leader_id FROM squad WHERE id = $4::uuid)),
        $4::uuid)
RETURNING id
`, agentID, runtimeID, issueID, squadID).Scan(&taskID); err != nil {
		t.Fatalf("queue task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
	return
}

// TestClaimTask_LeaderGetsBriefing — when the squad leader claims a task on
// a squad-assigned issue, the response's agent.instructions must include
// the Operating Protocol + Roster + user instructions.
func TestClaimTask_LeaderGetsBriefing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var leaderID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		testWorkspaceID,
	).Scan(&leaderID, &runtimeID); err != nil {
		t.Fatalf("get leader agent: %v", err)
	}

	squad := seedSquadForBriefing(t, leaderID, "Briefing Claim Squad", "Be terse.")

	helper := createHandlerTestAgent(t, "Briefing Helper", []byte("[]"))
	addAgentMember(t, squad.ID, helper, "implementer")

	queueSquadIssueTaskFor(t, util.UUIDToString(squad.ID), leaderID, runtimeID, 95001)

	agent := claimAndDecodeAgent(t, runtimeID)
	for _, want := range []string{
		"## Squad Operating Protocol",
		"## Squad Roster",
		"Leader (you):",
		"## Squad Instructions (Briefing Claim Squad)",
		"Be terse.",
		"`[@Briefing Helper](mention://agent/" + helper + ")`",
	} {
		if !strings.Contains(agent.Instructions, want) {
			t.Errorf("expected agent.instructions to contain %q\n--- instructions ---\n%s", want, agent.Instructions)
		}
	}
}

// TestClaimTask_NonLeaderGetsNoBriefing — when a non-leader squad member
// claims a task on a squad-assigned issue, NO briefing is injected.
func TestClaimTask_NonLeaderGetsNoBriefing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var leaderID string
	if err := testPool.QueryRow(ctx,
		`SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1`,
		testWorkspaceID,
	).Scan(&leaderID); err != nil {
		t.Fatalf("get leader agent: %v", err)
	}

	squad := seedSquadForBriefing(t, leaderID, "Non-Leader Squad", "Squad guidance.")

	// Create a second agent (NOT the leader) with its own runtime so the
	// claim path picks its task without ambiguity.
	helperID := createHandlerTestAgent(t, "Non Leader Helper", []byte("[]"))
	addAgentMember(t, squad.ID, helperID, "")
	var helperRuntime string
	if err := testPool.QueryRow(ctx,
		`SELECT runtime_id FROM agent WHERE id = $1`, helperID,
	).Scan(&helperRuntime); err != nil {
		t.Fatalf("get helper runtime: %v", err)
	}

	queueSquadIssueTaskFor(t, util.UUIDToString(squad.ID), helperID, helperRuntime, 95002)

	agent := claimAndDecodeAgent(t, helperRuntime)
	for _, mustNot := range []string{
		"Squad Operating Protocol",
		"Squad Roster",
		"Squad Instructions (Non-Leader Squad)",
	} {
		if strings.Contains(agent.Instructions, mustNot) {
			t.Errorf("non-leader claim should NOT contain %q\n--- instructions ---\n%s", mustNot, agent.Instructions)
		}
	}
}

// Avoid "imported and not used: pgtype" if helpers above are the only users.
var _ pgtype.UUID
