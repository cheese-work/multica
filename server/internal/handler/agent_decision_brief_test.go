package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// enableAgentDecisions turns the pilot setting on for the shared test
// workspace and restores the previous value afterwards.
func enableAgentDecisions(t *testing.T, on bool) {
	t.Helper()
	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
	})
	dbfx.Exec(t, `UPDATE workspace SET settings=jsonb_set(settings,'{agent_decisions_enabled}',to_jsonb($2::boolean)) WHERE id=$1`, testWorkspaceID, on)
}

type briefResponse struct {
	Enabled           bool `json:"enabled"`
	ReviewTargetHours int  `json:"review_target_hours"`
	Stopped           *struct {
		Reason string `json:"reason"`
	} `json:"stopped"`
	Decisions []struct {
		InstanceID     string `json:"instance_id"`
		Gate           string `json:"gate"`
		State          string `json:"state"`
		ReviewTargetAt string `json:"review_target_at"`
		VetoLine       string `json:"veto_line"`
		Link           string `json:"link"`
	} `json:"decisions"`
	NoticeFailures []struct {
		InstanceID string `json:"instance_id"`
		Attempts   int    `json:"attempts"`
		LastError  string `json:"last_error"`
	} `json:"notice_failures"`
	Pilot struct {
		Effective   int   `json:"effective"`
		Vetoed      int   `json:"vetoed"`
		Denominator int   `json:"denominator"`
		AsksAvoided int   `json:"asks_avoided"`
		RunTokens   int64 `json:"run_tokens"`
		Runs        int   `json:"runs"`
	} `json:"pilot"`
}

func readBrief(t *testing.T) briefResponse {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.GetAgentDecisionBrief(w, newRequest(http.MethodGet, "/api/agent-decisions/brief", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("brief status=%d: %s", w.Code, w.Body)
	}
	var out briefResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func prepareBrief(t *testing.T) agentDecisionFixture {
	t.Helper()
	f := prepareVeto(t)
	dbfx.Exec(t, `DELETE FROM decision_agent_notice WHERE issue_id=$1`, f.issueID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_agent_notice WHERE issue_id=$1`, f.issueID) })
	return f
}

// Two votes through the production comment path settle when the setting is on.
func TestAgentDecisionSettlesThroughCommentPathWhenEnabled(t *testing.T) {
	f := prepareBrief(t)
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.reveal(t, agent, "PASS", f.opinion(agent, "PASS"), http.StatusCreated)
	}
	if n := liveSettlements(t, f); n != 1 {
		t.Fatalf("enabled workspace settled %d records through comments, want 1", n)
	}
}

func TestAgentDecisionSettingOffSettlesNothing(t *testing.T) {
	f := prepareBrief(t)
	enableAgentDecisions(t, false)
	for _, agent := range []string{f.leaderID, f.peerID} {
		f.reveal(t, agent, "PASS", f.opinion(agent, "PASS"), http.StatusCreated)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatalf("setting off but %d settlements exist", n)
	}
	if err := settlementReveal(f, f.leaderID, f.opinion(f.leaderID, "PASS")); err == nil {
		// the direct engine run is a second reveal and is refused; either way nothing settles
		t.Log("second reveal accepted")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_settlement WHERE instance_id=$1`, f.instance); n != 0 {
		t.Fatalf("setting off but %d settlements exist after engine run", n)
	}
}

func TestAgentDecisionBriefListsRecordNotNeedsYou(t *testing.T) {
	f := prepareBrief(t)
	effective(t, f)
	brief := readBrief(t)
	if !brief.Enabled || brief.ReviewTargetHours != 24 {
		t.Fatalf("brief header: %+v", brief)
	}
	var found bool
	for _, d := range brief.Decisions {
		if d.InstanceID != f.instance {
			continue
		}
		found = true
		if d.State != "effective" || d.Gate != "screenshot_evidence" {
			t.Fatalf("record: %+v", d)
		}
		if !strings.Contains(d.VetoLine, "Veto: decision:"+f.instance+" "+boundAfter) {
			t.Fatalf("veto line: %q", d.VetoLine)
		}
		target, err := time.Parse(time.RFC3339, d.ReviewTargetAt)
		if err != nil || time.Until(target) < 23*time.Hour || time.Until(target) > 25*time.Hour {
			t.Fatalf("review target %q: %v", d.ReviewTargetAt, err)
		}
	}
	if !found {
		t.Fatalf("effective record missing from brief: %+v", brief.Decisions)
	}
	needs, err := testHandler.Queries.ListOpenDecisionRequestsForHuman(t.Context(), db.ListOpenDecisionRequestsForHumanParams{WorkspaceID: parseUUID(testWorkspaceID), HumanUserID: parseUUID(testUserID)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range needs {
		if uuidToString(r.ID) == f.instance {
			t.Fatal("agent record appeared in Needs you")
		}
	}
}

func TestAgentDecisionPilotMetrics(t *testing.T) {
	f := prepareBrief(t)
	effective(t, f)
	// A commit-reveal run: its tokens count toward the pilot cost.
	dbfx.Exec(t, `INSERT INTO task_usage(task_id,provider,model,input_tokens,output_tokens) VALUES($1,'p','m',100,50)`, f.tasks[f.leaderID])
	before := readBrief(t)
	if before.Pilot.AsksAvoided < 1 || before.Pilot.RunTokens < 150 || before.Pilot.Runs < 1 {
		t.Fatalf("pilot before veto: %+v", before.Pilot)
	}
	f.post(t, f.leaderID, f.vetoBody("metrics"), http.StatusCreated)
	after := readBrief(t)
	if after.Pilot.Vetoed != before.Pilot.Vetoed+1 || after.Pilot.AsksAvoided != before.Pilot.AsksAvoided-1 || after.Pilot.Denominator != before.Pilot.Denominator {
		t.Fatalf("pilot after veto: before=%+v after=%+v", before.Pilot, after.Pilot)
	}
}

func TestAgentDecisionNoticeDeliveredWithLinkAndVetoAction(t *testing.T) {
	f := prepareBrief(t)
	effective(t, f)
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_notice WHERE instance_id=$1 AND delivered_at IS NULL`, f.instance); n != 1 {
		t.Fatalf("outbox rows=%d, want 1 written with the settlement", n)
	}
	if err := testHandler.deliverAgentDecisionNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_notice WHERE instance_id=$1 AND delivered_at IS NOT NULL`, f.instance); n != 1 {
		t.Fatal("notice not marked delivered")
	}
	var body string
	dbfx.QueryRow(t, `SELECT body FROM inbox_item WHERE issue_id=$1 AND type='agent_decision_notice' AND recipient_type='member' LIMIT 1`, f.issueID).Scan(&body)
	if !strings.Contains(body, "Veto: decision:"+f.instance+" "+boundAfter) || !strings.Contains(body, "/issues/") {
		t.Fatalf("notice body lacks veto action or record link: %q", body)
	}
	// Delivered once: a second pass adds nothing.
	if err := testHandler.deliverAgentDecisionNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND type='agent_decision_notice' AND recipient_type='member' AND recipient_id=$2`, f.issueID, testUserID); n != 1 {
		t.Fatalf("owner notices=%d, want 1", n)
	}
}

func TestAgentDecisionNoticeFailureIsRetriedAndShownInBrief(t *testing.T) {
	f := prepareBrief(t)
	effective(t, f)
	var roles []struct{ id, role string }
	rows, err := testPool.Query(t.Context(), `SELECT id::text,role FROM member WHERE workspace_id=$1 AND role='owner'`, testWorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r struct{ id, role string }
		rows.Scan(&r.id, &r.role)
		roles = append(roles, r)
	}
	rows.Close()
	dbfx.Exec(t, `UPDATE member SET role='admin' WHERE workspace_id=$1 AND role='owner'`, testWorkspaceID)
	restore := func() {
		for _, r := range roles {
			dbfx.Exec(t, `UPDATE member SET role='owner' WHERE id=$1`, r.id)
		}
	}
	t.Cleanup(restore)
	_ = testHandler.deliverAgentDecisionNotices(t.Context())
	var attempts int
	var lastError string
	dbfx.QueryRow(t, `SELECT attempts,last_error FROM decision_agent_notice WHERE instance_id=$1 AND delivered_at IS NULL`, f.instance).Scan(&attempts, &lastError)
	if attempts != 1 || lastError == "" {
		t.Fatalf("failure not recorded: attempts=%d error=%q", attempts, lastError)
	}
	brief := readBrief(t)
	var shown bool
	for _, n := range brief.NoticeFailures {
		shown = shown || (n.InstanceID == f.instance && n.Attempts == 1 && n.LastError != "")
	}
	if !shown {
		t.Fatalf("delivery failure missing from brief: %+v", brief.NoticeFailures)
	}
	// Backoff holds the retry until it is due.
	_ = testHandler.deliverAgentDecisionNotices(t.Context())
	dbfx.QueryRow(t, `SELECT attempts FROM decision_agent_notice WHERE instance_id=$1`, f.instance).Scan(&attempts)
	if attempts != 1 {
		t.Fatalf("retried before backoff elapsed: attempts=%d", attempts)
	}
	restore()
	dbfx.Exec(t, `UPDATE decision_agent_notice SET next_attempt_at=now() WHERE instance_id=$1`, f.instance)
	if err := testHandler.deliverAgentDecisionNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM decision_agent_notice WHERE instance_id=$1 AND delivered_at IS NOT NULL`, f.instance); n != 1 {
		t.Fatal("retry did not deliver")
	}
}

// One excluded-category decision turns the pilot off; existing records stay vetoable.
func TestAgentDecisionStopRule(t *testing.T) {
	f := prepareBrief(t)
	effective(t, f)
	scope := map[string]any{}
	for k, v := range f.scope {
		scope[k] = v
	}
	scope["waived_gate"] = "merge_approval"
	raw, _ := json.Marshal(scope)
	body := "Proposal: decision:" + uuid.NewString() + " " + boundAfter + "\n\n```decision\n" + string(raw) + "\n```\n"
	f.post(t, f.writerID, body, http.StatusBadRequest)
	brief := readBrief(t)
	if brief.Enabled || brief.Stopped == nil || !strings.Contains(brief.Stopped.Reason, "merge_approval") {
		t.Fatalf("stop rule did not turn the setting off: %+v", brief)
	}
	f.post(t, f.leaderID, f.vetoBody("still vetoable after stop"), http.StatusCreated)
	if n := liveSettlements(t, f); n != 0 {
		t.Fatalf("record not vetoable after stop: %d live", n)
	}
}

func TestAgentDecisionSettingIsOwnerOnly(t *testing.T) {
	enableAgentDecisions(t, false)
	set := func(userID string, enabled bool) int {
		req := newRequest(http.MethodPut, "/api/agent-decisions/setting", map[string]any{"enabled": enabled})
		req.Header.Set("X-User-ID", userID)
		w := httptest.NewRecorder()
		testHandler.SetAgentDecisionSetting(w, req)
		return w.Code
	}
	if code := set(testUserID, true); code != http.StatusOK {
		t.Fatalf("owner set status=%d", code)
	}
	if !readBrief(t).Enabled {
		t.Fatal("owner setting did not stick")
	}
	if code := set("00000000-0000-0000-0000-000000000000", false); code == http.StatusOK {
		t.Fatal("non-member changed the setting")
	}
}
