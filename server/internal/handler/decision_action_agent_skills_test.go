package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// skillsScenario is an agent with some of three workspace skills assigned. The
// target is the asking agent itself (self) or another agent.
type skillsScenario struct {
	fixture singleCallFixture
	target  string
	skills  [3]string // skills[0] is "A", skills[1] "B", skills[2] "C"
}

// newSkillsScenario assigns the skills at enabled and disabled (indexes into
// the three skills) to the target.
func newSkillsScenario(test *testing.T, self bool, enabled, disabled []int) skillsScenario {
	test.Helper()
	scenario := skillsScenario{fixture: newActionFixture(test)}
	scenario.target = scenario.fixture.agentID
	if !self {
		scenario.target = createHandlerTestAgent(test, "skills-target-"+uuid.NewString()[:8], nil)
	}
	dbfx.Exec(test, `DELETE FROM agent_skill WHERE agent_id = $1`, scenario.target)
	for i := range scenario.skills {
		dbfx.QueryRow(test, `INSERT INTO skill (workspace_id, name, description, content) VALUES ($1, $2, 'd', 'c') RETURNING id`,
			testWorkspaceID, fmt.Sprintf("skill-%c-%s", 'a'+i, uuid.NewString()[:8])).Scan(&scenario.skills[i])
		dbfx.Cleanup(test, `DELETE FROM skill WHERE id = $1`, scenario.skills[i])
	}
	dbfx.Cleanup(test, `DELETE FROM agent_skill WHERE agent_id = $1`, scenario.target)
	for _, i := range enabled {
		dbfx.Exec(test, `INSERT INTO agent_skill (agent_id, skill_id) VALUES ($1, $2)`, scenario.target, scenario.skills[i])
	}
	for _, i := range disabled {
		dbfx.Exec(test, `INSERT INTO agent_skill (agent_id, skill_id, enabled) VALUES ($1, $2, FALSE)`, scenario.target, scenario.skills[i])
	}
	return scenario
}

func (scenario skillsScenario) ids(indexes ...int) []string {
	ids := make([]string, len(indexes))
	for i, index := range indexes {
		ids[i] = scenario.skills[index]
	}
	return ids
}

// params builds the action params for the after set with the server's digests.
func (scenario skillsScenario) params(test *testing.T, after ...int) map[string]any {
	test.Helper()
	proposed := make([]pgtype.UUID, len(after))
	for i, index := range after {
		proposed[i] = parseUUID(scenario.skills[index])
	}
	snapshot, err := loadSkillSetSnapshot(context.Background(), testHandler.Queries, parseUUID(testWorkspaceID), parseUUID(scenario.target), proposed)
	if err != nil {
		test.Fatal(err)
	}
	return map[string]any{
		"agent_id": scenario.target, "skill_ids": scenario.ids(after...),
		"base_sha256": snapshot.LiveSHA256, "after_sha256": snapshot.AfterSHA256,
	}
}

type skillsRequest struct {
	scenario skillsScenario
	action   pendingAction
}

// propose files agent.skills.set with the after set; edit may break the params.
func (scenario skillsScenario) propose(test *testing.T, status int, edit func(params map[string]any), after ...int) (skillsRequest, string) {
	test.Helper()
	params := scenario.params(test, after...)
	if edit != nil {
		edit(params)
	}
	var created actionResponse
	body := scenario.fixture.respond(test, scenario.fixture.withAction(map[string]any{"type": "agent.skills.set", "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := scenario.fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return skillsRequest{scenario: scenario}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return skillsRequest{scenario, pendingAction{scenario.fixture, created.ID, created.Action.ID}}, body
}

// live is the target's assignment: skill id to enabled flag.
func (scenario skillsScenario) live(test *testing.T) map[string]bool {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT skill_id::text, enabled FROM agent_skill WHERE agent_id = $1`, scenario.target)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	live := map[string]bool{}
	for rows.Next() {
		var id string
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			test.Fatal(err)
		}
		live[id] = enabled
	}
	return live
}

// wantLive checks the assignment holds exactly the given skills, all enabled.
func (scenario skillsScenario) wantLive(test *testing.T, indexes ...int) {
	test.Helper()
	want := map[string]bool{}
	for _, id := range scenario.ids(indexes...) {
		want[id] = true
	}
	if got := scenario.live(test); fmt.Sprint(got) != fmt.Sprint(want) {
		test.Fatalf("assignment %v, want %v", got, want)
	}
}

func (request skillsRequest) claim(test *testing.T) map[string]any {
	test.Helper()
	var preview string
	dbfx.QueryRow(test, `SELECT preconditions->>'preview_sha256' FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&preview)
	claim := request.action.claim(test, map[string]string{})
	claim["preview_sha256"] = preview
	return claim
}

func (request skillsRequest) approveOwner(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	makeApproverOwner(test, request.action, "owner")
	return request.action.approve(test, handler, request.claim(test), status)
}

// assertNotApplied proves a refused approval changed nothing.
func (request skillsRequest) assertNotApplied(test *testing.T) {
	test.Helper()
	request.action.assertUntouched(test)
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

func TestAgentSkillsSetIsRegisteredAsAnOwnerOnlyPreviewedAgentAction(test *testing.T) {
	definition, found := decisionaction.Lookup("agent.skills.set", 1)
	if !found {
		test.Fatal("agent.skills.set@1 is not registered")
	}
	if definition.Target != decisionaction.TargetAgent || !definition.OwnerOnly || !definition.RequiresPreview || definition.MinClass != "spend" {
		test.Fatalf("definition %+v; want agent target, owner only, spend, requires preview", definition)
	}
	if _, found := configChanges["agent.skills.set"]; !found {
		test.Fatal("agent.skills.set has no config-change core")
	}
}

func TestAgentSkillsSetRequestStoresTheServerPreview(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		test.Fatal(err)
	}
	var field, target string
	_ = json.Unmarshal(fields["field"], &field)
	_ = json.Unmarshal(fields["target_id"], &target)
	if field != "agent.skills" || target != scenario.target {
		test.Fatalf("preconditions %s lack field or target", raw)
	}
	var preview struct {
		DiffKind string   `json:"diff_kind"`
		Before   []string `json:"before"`
		After    []string `json:"after"`
	}
	var shown struct {
		Preview json.RawMessage `json:"preview"`
	}
	_ = json.Unmarshal([]byte(raw), &shown)
	if err := json.Unmarshal(shown.Preview, &preview); err != nil || preview.DiffKind != "lines" || len(preview.Before) != 1 || len(preview.After) != 2 {
		test.Fatalf("preview %s (%v); want one live and two new lines", shown.Preview, err)
	}
	for _, line := range preview.After {
		if !strings.HasSuffix(line, " [enabled]") || !strings.Contains(line, " (") {
			test.Fatalf("after line %q; want `name (id) [enabled]`", line)
		}
	}
}

func TestAgentSkillsSetRequestRefusals(test *testing.T) {
	cases := map[string]struct {
		status int
		code   string
		edit   func(scenario skillsScenario, params map[string]any)
		after  []int
	}{
		"no change":                  {http.StatusBadRequest, "no_change", nil, []int{0}},
		"foreign skill":              {http.StatusNotFound, "skill_not_found", func(_ skillsScenario, p map[string]any) { p["skill_ids"] = []string{foreignWorkspaceSkill(test)} }, []int{0}},
		"missing skill":              {http.StatusNotFound, "skill_not_found", func(_ skillsScenario, p map[string]any) { p["skill_ids"] = []string{uuid.NewString()} }, []int{0}},
		"after digest is wrong":      {http.StatusBadRequest, "after_digest_mismatch", func(_ skillsScenario, p map[string]any) { p["after_sha256"] = strings.Repeat("0", 64) }, []int{0, 1}},
		"base digest is stale":       {http.StatusConflict, "precondition_drift", func(_ skillsScenario, p map[string]any) { p["base_sha256"] = strings.Repeat("0", 64) }, []int{0, 1}},
		"field outside the action":   {http.StatusBadRequest, "invalid_params", func(_ skillsScenario, p map[string]any) { p["enabled"] = true }, []int{0, 1}},
		"skill id is not a uuid":     {http.StatusBadRequest, "invalid_params", func(_ skillsScenario, p map[string]any) { p["skill_ids"] = []string{"nope"} }, []int{0, 1}},
		"agent of another workspace": {http.StatusForbidden, "target_workspace", func(_ skillsScenario, p map[string]any) { p["agent_id"] = foreignWorkspaceAgent(test) }, []int{0, 1}},
	}
	for name, tc := range cases {
		test.Run(name, func(test *testing.T) {
			scenario := newSkillsScenario(test, false, []int{0}, nil)
			var edit func(map[string]any)
			if tc.edit != nil {
				edit = func(params map[string]any) { tc.edit(scenario, params) }
			}
			_, body := scenario.propose(test, tc.status, edit, tc.after...)
			mustContain(test, body, tc.code)
		})
	}
}

func foreignWorkspaceSkill(test *testing.T) string {
	test.Helper()
	var id string
	dbfx.QueryRow(test, `INSERT INTO skill (workspace_id, name) VALUES ($1, $2) RETURNING id`, foreignWorkspaceID(test), "foreign-"+uuid.NewString()[:8]).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM skill WHERE id = $1`, id)
	return id
}

func TestAgentSkillsSetRefusesAnArchivedAgent(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, scenario.target)
	_, body := scenario.propose(test, http.StatusConflict, nil, 0, 1)
	mustContain(test, body, "agent_archived")
}

func TestAgentSkillsSetRefusesSkillsOnAQoderAgent(test *testing.T) {
	scenario := newSkillsScenario(test, false, nil, nil)
	runtimeID := dbfx.Runtime(test, "QSK", map[string]any{"provider": "qoder_cloud", "runtime_mode": "cloud"})
	dbfx.Exec(test, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, scenario.target)
	_, body := scenario.propose(test, http.StatusBadRequest, nil, 0)
	mustContain(test, body, "Qoder")
}

func TestAgentSkillsSetApproveAddsOneSkill(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	scenario.wantLive(test, 0, 1)
	var raw string
	dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result["readback_sha256"] != scenario.params(test, 0, 1)["base_sha256"] {
		test.Fatalf("result %s (%v); want the readback of the new assignment", raw, err)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d, want 1", n)
	}
}

func TestAgentSkillsSetApproveRemovesOneSkill(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0, 1}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	scenario.wantLive(test, 0)
}

func TestAgentSkillsSetApproveEmptiesTheList(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0, 1}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	scenario.wantLive(test)
}

func TestAgentSkillsSetApproveRefusals(test *testing.T) {
	test.Run("a file of a skill being added is edited after the request", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
		dbfx.Exec(test, `INSERT INTO skill_file (skill_id, path, content) VALUES ($1, 'n.md', 'new')`, scenario.skills[1])
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test)
		scenario.wantLive(test, 0)
	})
	test.Run("an assigned skill file is edited after the request", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0, 1}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0)
		dbfx.Exec(test, `INSERT INTO skill_file (skill_id, path, content) VALUES ($1, 'n.md', 'new')`, scenario.skills[0])
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test)
	})
	test.Run("an enabled flag changes after the request", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0, 1}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0)
		dbfx.Exec(test, `UPDATE agent_skill SET enabled = FALSE WHERE agent_id = $1 AND skill_id = $2`, scenario.target, scenario.skills[1])
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test)
	})
	test.Run("mixed assignment flips an enabled flag at approval", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0}, []int{1})
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 2)
		flip := func(ctx context.Context, queries *db.Queries, _ ApprovalClaim) (pgtype.UUID, error) {
			_, err := queries.SetAgentSkillEnabled(ctx, db.SetAgentSkillEnabledParams{AgentID: parseUUID(scenario.target), SkillID: parseUUID(scenario.skills[1]), Enabled: true})
			return pgtype.UUID{}, err
		}
		makeApproverOwner(test, request.action, "owner")
		mustContain(test, request.action.approve(test, withVerifier(flip), request.claim(test), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test)
	})
	test.Run("approver is an admin", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
		makeApproverOwner(test, request.action, "admin")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test)
		scenario.wantLive(test, 0)
	})
	test.Run("the agent is archived after the request", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
		dbfx.Exec(test, `UPDATE agent SET archived_at = now() WHERE id = $1`, scenario.target)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "agent_archived")
		request.assertNotApplied(test)
	})
}

func TestAgentSkillsSetSelfTarget(test *testing.T) {
	test.Run("adding a skill is refused at the request", func(test *testing.T) {
		scenario := newSkillsScenario(test, true, []int{0}, nil)
		_, body := scenario.propose(test, http.StatusConflict, nil, 0, 1)
		mustContain(test, body, "self_enable_refused")
	})
	test.Run("[A enabled, B disabled] to [B] is refused at the request", func(test *testing.T) {
		scenario := newSkillsScenario(test, true, []int{0}, []int{1})
		_, body := scenario.propose(test, http.StatusConflict, nil, 1)
		mustContain(test, body, "self_enable_refused")
	})
	test.Run("an enable flag turned off between request and approval is refused", func(test *testing.T) {
		scenario := newSkillsScenario(test, true, []int{0, 1}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 1)
		dbfx.Exec(test, `UPDATE agent_skill SET enabled = FALSE WHERE agent_id = $1 AND skill_id = $2`, scenario.target, scenario.skills[1])
		// The live digest moved, so this is drift; either way nothing is enabled.
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test)
	})
	test.Run("removing an enabled skill is accepted", func(test *testing.T) {
		scenario := newSkillsScenario(test, true, []int{0}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil)
		request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
		scenario.wantLive(test)
	})
	test.Run("keeping A and removing B is accepted", func(test *testing.T) {
		scenario := newSkillsScenario(test, true, []int{0, 1}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0)
		request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
		scenario.wantLive(test, 0)
	})
}

func TestAgentSkillsSetOwnerMayEnableADisabledSkillOnAnotherAgent(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, []int{1})
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions->'preview'->>'diff' FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	if !strings.Contains(raw, "[disabled]") || !strings.Contains(raw, "[enabled]") {
		test.Fatalf("diff %s; want the disabled line replaced by an enabled one", raw)
	}
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	scenario.wantLive(test, 0, 1)
}

func TestAgentSkillsSetListDiffHasOnlyChangedLines(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0, 1}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 2)
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions->'preview'->>'diff' FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var hunks []struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal([]byte(raw), &hunks); err != nil {
		test.Fatal(err)
	}
	var changed []string
	for _, hunk := range hunks {
		for _, line := range hunk.Lines {
			if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
				changed = append(changed, line)
			}
		}
	}
	if len(changed) != 2 || !strings.Contains(changed[0], scenario.skills[1]) || !strings.HasPrefix(changed[0], "-") ||
		!strings.Contains(changed[1], scenario.skills[2]) || !strings.HasPrefix(changed[1], "+") {
		test.Fatalf("changed lines %q; want -B and +C only", changed)
	}
	if strings.Contains(strings.Join(changed, "\n"), scenario.skills[0]) {
		test.Fatalf("unchanged skill A is in the changed lines %q", changed)
	}
}

func TestAgentSkillsSetPublishesTheAgentEventOnce(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	var mutex sync.Mutex
	var seen []map[string]any
	testHandler.Bus.Subscribe(protocol.EventAgentStatus, func(event events.Event) {
		raw, _ := json.Marshal(event.Payload)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if payload["agent_id"] != scenario.target {
			return
		}
		mutex.Lock()
		seen = append(seen, payload)
		mutex.Unlock()
	})
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	mutex.Lock()
	defer mutex.Unlock()
	if len(seen) != 1 {
		test.Fatalf("agent:status events for the target = %d, want 1: %v", len(seen), seen)
	}
	skills, _ := seen[0]["skills"].([]any)
	if len(skills) != 2 {
		test.Fatalf("event skills %v; want the two assigned skills", seen[0]["skills"])
	}
}

// A trigger that stores the row disabled makes the stored assignment differ
// from the approved one; the readback must catch it and undo the write.
func TestAgentSkillsSetReadbackMismatchUndoesTheWrite(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	fn := "che4488_disable_" + strings.ReplaceAll(scenario.target, "-", "")
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.enabled := FALSE; RETURN NEW; END $$`, fn),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON agent_skill FOR EACH ROW WHEN (NEW.agent_id = '%s') EXECUTE FUNCTION %s()`, fn, scenario.target, fn),
	} {
		dbfx.Exec(test, stmt)
	}
	test.Cleanup(func() {
		dbfx.Exec(test, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_skill`, fn))
		dbfx.Exec(test, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if code := request.action.failureCode(test); code != "readback_mismatch" {
		test.Fatalf("failure code %q, want readback_mismatch", code)
	}
	if got := scenario.live(test); len(got) != 1 || !got[scenario.skills[0]] {
		test.Fatalf("assignment %v after a readback mismatch, want the original [A enabled]", got)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

func TestAgentSkillsSetBusyWhileAWriterHoldsALock(test *testing.T) {
	test.Run("an ordinary assignment edit holds the agent row", func(test *testing.T) {
		scenario := newSkillsScenario(test, false, []int{0}, nil)
		request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
		holdRow(test, `UPDATE agent SET description = 'ordinary edit' WHERE id = $1`, scenario.target)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "busy_retry")
		request.assertNotApplied(test)
	})
}

// skillFileCase is one skill-file writer against the skill of an approval: the
// skill is either already assigned (kept) or the one being added.
type skillFileCase struct {
	scenario skillsScenario
	request  skillsRequest
	file     skillWriterFixture
}

// newSkillFileCase puts a file a.md ("old") on the skill before the request,
// so the request's digests include it.
func newSkillFileCase(test *testing.T, assigned bool) skillFileCase {
	test.Helper()
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	index, after := 1, []int{0, 1}
	if assigned {
		index, after = 0, []int{0, 2}
	}
	skillID := scenario.skills[index]
	fileID := dbfx.Insert(test, "skill_file", testutil.Cols{"skill_id": skillID, "path": "a.md", "content": "old"})
	request, _ := scenario.propose(test, http.StatusCreated, nil, after...)
	return skillFileCase{scenario, request, skillWriterFixture{skillID: skillID, fileID: fileID}}
}

func eachSkillFileCase(test *testing.T, body func(test *testing.T, writer skillWriter, c skillFileCase)) {
	for _, assigned := range []bool{true, false} {
		kind := "a skill being added"
		if assigned {
			kind = "an assigned skill"
		}
		for _, writer := range skillFileWriters() {
			test.Run(kind+"/"+writer.name, func(test *testing.T) {
				body(test, writer, newSkillFileCase(test, assigned))
			})
		}
	}
}

// Writer first: the approval answers busy_retry while the writer is in flight,
// and precondition_drift once the writer has committed (CHE-4484 carry-over).
func TestAgentSkillsSetApprovalAfterASkillFileWriter(test *testing.T) {
	eachSkillFileCase(test, func(test *testing.T, writer skillWriter, c skillFileCase) {
		release := writer.blockOn(test, c.file)
		done := runAsync(test, writer, c.file)
		waitForSkillFileLockWaiter(test)
		mustContain(test, c.request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "busy_retry")
		c.request.assertNotApplied(test)
		release()
		if status := <-done; status != writer.wantStatus {
			test.Fatalf("%s status = %d, want %d", writer.name, status, writer.wantStatus)
		}
		if !writer.settled(test, c.file) {
			test.Fatalf("%s change not visible after commit", writer.name)
		}
		mustContain(test, c.request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		c.request.assertNotApplied(test)
	})
}

// Approval first: a real approval is parked inside its transaction, after it
// took its locks and read the skill. The writer waits and commits after the
// approval, which applied the assignment it was asked for.
func TestAgentSkillsSetSkillFileWriterWaitsForTheApproval(test *testing.T) {
	eachSkillFileCase(test, func(test *testing.T, writer skillWriter, c skillFileCase) {
		original, executor := configChanges[agentSkillsAction], decisionActionExecutors[agentSkillsAction]
		entered, proceed := make(chan struct{}), make(chan struct{})
		parked := original
		parked.Apply = func(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
			close(entered)
			<-proceed
			return original.Apply(ctx, execution, read)
		}
		decisionActionExecutors[agentSkillsAction] = func(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
			return executeConfigChange(ctx, parked, execution)
		}
		test.Cleanup(func() { decisionActionExecutors[agentSkillsAction] = executor })
		makeApproverOwner(test, c.request.action, "owner")
		claim := c.request.claim(test)
		approved := make(chan int, 1)
		go func() {
			status, body := approveClaim(test, withVerifier(acceptingVerifier), agentInstructionRequest{action: c.request.action}, claim)
			if status != http.StatusOK {
				test.Errorf("approval status = %d (%s), want 200", status, body)
			}
			approved <- status
		}()
		select {
		case <-entered:
		case status := <-approved:
			test.Fatalf("approval answered %d without reaching the write", status)
		}
		done := runAsync(test, writer, c.file)
		select {
		case status := <-done:
			test.Fatalf("%s returned %d while the approval held the skill", writer.name, status)
		case <-time.After(300 * time.Millisecond):
		}
		close(proceed)
		if status := <-approved; status != http.StatusOK {
			test.Fatalf("approval status = %d, want 200", status)
		}
		if status := <-done; status != writer.wantStatus {
			test.Fatalf("%s status = %d, want %d", writer.name, status, writer.wantStatus)
		}
		if !writer.settled(test, c.file) {
			test.Fatalf("%s change not visible after both committed", writer.name)
		}
		if n := c.request.action.activityCount(test, "config_change_applied"); n != 1 {
			test.Fatalf("config_change_applied rows = %d, want 1", n)
		}
	})
}

// The card quotes the agent's skills, so it is withheld for an agent the
// request's human cannot view.
func TestAgentSkillsSetCardPreviewIsWithheldForAnAgentTheHumanCannotSee(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	if card := request.action.cardAction(test); card == nil || card.ConfigChange == nil || card.ConfigChange.Field != agentSkillsField {
		test.Fatalf("card preview before the agent is hidden: %+v", card)
	}
	dbfx.Exec(test, `UPDATE agent SET owner_id = $2, permission_mode = 'private' WHERE id = $1`, scenario.target, testUserID)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, request.action.humanID)
	if card := request.action.cardAction(test); card == nil || card.ConfigChange != nil {
		test.Fatalf("card preview of an agent the human cannot view: %+v", card)
	}
}

func TestAgentSkillsSetRunsOnce(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	request, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	handler := withVerifier(acceptingVerifier)
	request.approveOwner(test, handler, http.StatusOK)
	request.action.approve(test, handler, request.claim(test), http.StatusConflict)
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
	}
}

// Two approvals racing on one agent apply once; the loser sees the winner's
// committed assignment and answers precondition_drift.
func TestAgentSkillsSetConcurrentApprovalsApplyOnce(test *testing.T) {
	scenario := newSkillsScenario(test, false, []int{0}, nil)
	first, _ := scenario.propose(test, http.StatusCreated, nil, 0, 1)
	other := scenario
	other.fixture = newActionFixture(test)
	second, _ := other.propose(test, http.StatusCreated, nil, 0, 2)
	makeApproverOwner(test, first.action, "owner")
	makeApproverOwner(test, second.action, "owner")
	handler := withVerifier(acceptingVerifier)
	requests := []skillsRequest{first, second}
	claims := []map[string]any{first.claim(test), second.claim(test)}
	statuses, bodies := make([]int, 2), make([]string, 2)
	var group sync.WaitGroup
	for i, request := range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[i], bodies[i] = approveClaim(test, handler, agentInstructionRequest{action: request.action}, claims[i])
		}()
	}
	group.Wait()
	winner, loser := 0, 1
	if statuses[1] == http.StatusOK {
		winner, loser = 1, 0
	}
	if statuses[winner] != http.StatusOK || statuses[loser] != http.StatusConflict || !strings.Contains(bodies[loser], "precondition_drift") {
		test.Fatalf("statuses %v (%q, %q); want one 200 and one 409 precondition_drift", statuses, bodies[0], bodies[1])
	}
	applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
	if applied != 1 {
		test.Fatalf("applied %d times (statuses %v), want exactly 1", applied, statuses)
	}
	want := []int{0, 1}
	if winner == 1 {
		want = []int{0, 2}
	}
	scenario.wantLive(test, want...)
	if !slices.Contains(statuses, http.StatusOK) {
		test.Fatal("no approval succeeded")
	}
}
