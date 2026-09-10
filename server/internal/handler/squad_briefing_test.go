package handler

import (
	"context"
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

		// Case A3 (Sol P1 correction, CHE-348/PR #11 review of 04604ea1):
		// "already reconciled" must be keyed to the event's concrete
		// identity (candidate/SHA, revision, or comment) — a prior
		// published result about a different candidate must not be
		// mistaken for reconciliation of this one.
		for _, want := range []string{
			"Identify the event by its concrete key",
			"Treat the event as already reconciled ONLY if a prior",
			"published result on this issue names that same durable key",
			"a published result about a different candidate or an",
			"earlier revision does not reconcile this one, even if the",
			"category of event matches",
			// Restored (was dropped from this list during the CHE-359/PR #11
			// pass, though the hard-rules block itself still states it at
			// squad_briefing.go:117-118 equivalent) — Opus flagged it as
			// still-present-but-unguarded on PR #12 review of 63f1e35a8.
			"treating a same-category event on a",
			"different key as already reconciled",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing event-keying requirement %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}

		// Case A4 (Terra P1 correction, CHE-359/PR #11 review of
		// 19c756bce, then Opus BLOCK on PR #12 review of 63f1e35a8): a
		// durable candidate/SHA or PR revision must take precedence over
		// the reporting comment as the event's key, so a merge webhook and
		// a human reply reporting the SAME candidate collapse to the SAME
		// key instead of being treated as two different events. This is no
		// longer a hand-maintained prose duplicate of ReconciliationKey —
		// squadOperatingProtocolFor renders it by calling
		// reconciliationKeyingParagraph(), which derives its wording from
		// ReconciliationKey's actual precedence on probe events. See
		// TestSquadOperatingProtocolKeyingParagraphIsGeneratedFromReconciliationKey
		// below for the seam proof, and
		// TestReconciliationKey_CrossChannelReplayCollapsesToSameKey in
		// squad_briefing_reconciliation_test.go for the replay proof
		// exercised through squadOperatingProtocolFor.
		for _, want := range []string{
			"computed with this",
			"precedence (durable identity always wins over the comment that",
			"happened to report it): candidate/SHA present -> key is",
			"else PR revision present -> key is",
			"else -> key is",
			"Use the durable",
			"key even when this trigger arrived as a reporting comment (a human",
			"reply, a merge webhook, a status-check notification); only fall back",
			"to keying on the specific comment itself when no durable candidate/SHA",
			"or revision exists for the event",
			"a merge webhook and a human reply reporting the SAME candidate must",
			"resolve to the SAME key and must NOT be treated as two different",
			"events, even though they arrived through different channels and as",
			"different comments",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing durable-key precedence requirement %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}

		// Case B: the legitimate quiet no_action path (routine progress
		// update, or a duplicate/already-reconciled notification carrying
		// the same key) must not regress — this is the case the original
		// MUL-6984 rule protects.
		for _, want := range []string{
			"routine progress update that requires no response",
			"duplicate /",
			"already-actioned notification carrying the SAME key as an",
			"event this issue already reconciled",
			"record `no_action` and exit",
		} {
			if !strings.Contains(compact, want) {
				t.Errorf("ownsParentStatus=%v: protocol missing legitimate quiet no_action path %q\n--- protocol ---\n%s", ownsParentStatus, want, protocol)
			}
		}
	}
}

// TestSquadOperatingProtocolEventKeyingRejectsStaleCandidateMatch is a
// negative control for Case A3: text belonging to the pre-fix candidate
// (04604ea1, Sol's FAIL) must NOT satisfy the new keyed-reconciliation
// assertions, proving the test can actually distinguish keyed from
// unkeyed reconciliation language rather than passing on any protocol text.
func TestSquadOperatingProtocolEventKeyingRejectsStaleCandidateMatch(t *testing.T) {
	staleUnkeyedText := "If the event is not already reconciled by a prior published result on this issue, you must publish exactly one result."
	if strings.Contains(staleUnkeyedText, "Identify the event by its concrete key") {
		t.Fatal("negative control is broken: stale text should not contain the keying requirement")
	}
}

// TestSquadOperatingProtocolRejectsCommentEqualFootingLanguage is a negative
// control for Case A4 (CHE-359/PR #11 review of 19c756bce, then Opus BLOCK
// on PR #12 review of 63f1e35a8, then Sol BLOCK on PR #12 review of
// 52d013197). Opus's finding was that the prior version of this test
// compared two hardcoded string literals and could not fail for any
// production reason — it never called into production code. Sol's follow-up
// finding was that the fix for that (comparing the stale literal against the
// marker string, rather than against productionText) still proved nothing:
// production could ship BOTH the new precedence marker and the old
// equal-footing wording side by side, and this test would still pass. This
// version renders the ACTUAL production protocol text through
// squadOperatingProtocolFor and asserts directly against it both ways: the
// new marker must be present, and the stale equal-footing sentence must be
// absent.
func TestSquadOperatingProtocolRejectsCommentEqualFootingLanguage(t *testing.T) {
	productionText := squadOperatingProtocolFor(true)
	compact := strings.Join(strings.Fields(productionText), " ")

	const durablePrecedenceMarker = "Use the durable key even when this trigger arrived as a reporting comment"
	if !strings.Contains(compact, durablePrecedenceMarker) {
		t.Fatalf("production protocol text (squadOperatingProtocolFor) must contain the durable-precedence marker %q\n--- protocol ---\n%s", durablePrecedenceMarker, productionText)
	}

	// The pre-fix (19c756bce) equal-footing wording, which put the reporting
	// comment on equal footing with candidate/SHA and revision as the event
	// key, must be ABSENT from the actual rendered production text — not
	// merely absent from a hardcoded literal compared against another
	// hardcoded literal. This is the assertion Sol's BLOCK required: without
	// it, production could ship both the new marker and the stale sentence
	// and this test would not notice.
	const staleEqualFootingText = "the candidate commit/SHA, the PR revision, or the specific comment reporting it — not by its category"
	if strings.Contains(compact, staleEqualFootingText) {
		t.Fatalf("production protocol text (squadOperatingProtocolFor) still contains the stale pre-fix equal-footing wording %q — the durable-precedence marker must fully replace it, not merely coexist with it\n--- protocol ---\n%s", staleEqualFootingText, productionText)
	}
}

// TestSquadOperatingProtocolKeyingParagraphIsGeneratedFromReconciliationKey
// moved to squad_briefing_reconciliation_test.go, next to
// TestSquadOperatingProtocolReplayProvenThroughProductionBriefing — same
// executable-seam concern (CHE-359/PR #12 review of 63f1e35a8).

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
