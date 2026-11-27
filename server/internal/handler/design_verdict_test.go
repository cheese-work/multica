package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/designguard"
	"github.com/multica-ai/multica/server/internal/doopread"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Typed design review verdict record (CHE-7064 D1.4, S11): POST
// /api/issues/{id}/design-verdicts, task token only. The doop side is a fake
// that serves attestation views the case chooses.

const (
	verdictDesignPropertyID = "b73a1103-30d7-4f9b-9412-4edcd64f5232"
	verdictDoopOrigin       = "https://doop.example.com"
	verdictAttestation      = "run-1"
)

// verdictToken is a shared token of the shortest accepted length; built, not
// written out, so no secret-shaped literal sits in the repo.
var verdictToken = strings.Repeat("t", doopread.MinTokenLen)

// fakeDoop serves GET /api/service/design-attestations/{id}.
type fakeDoop struct {
	mu     sync.Mutex
	status int // 0 means 200
	view   map[string]any
	reads  int
}

func (f *fakeDoop) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(f.view)
}

// verdictFixture is an issue bound to a doop page, an author task of one lab and
// agents of both labs.
type verdictFixture struct {
	issueID                         string
	claudeRuntime, codexRuntime     string
	authorID, sameLabID, crossLabID string
	authorTask                      string
	doop                            *fakeDoop
}

func newVerdictFixture(t *testing.T) *verdictFixture {
	t.Helper()
	t.Setenv("MULTICA_VOTE_GATE_LABS", voteLabs)
	f := &verdictFixture{doop: &fakeDoop{}}
	f.claudeRuntime = dbfx.Runtime(t, "che7090-claude-"+safeTestName(t), testutil.Cols{"provider": "claude"})
	f.codexRuntime = dbfx.Runtime(t, "che7090-codex-"+safeTestName(t), testutil.Cols{"provider": "codex"})
	agent := func(name, runtimeID string) string {
		id := createHandlerTestAgent(t, name+"-"+safeTestName(t), nil)
		dbfx.Exec(t, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, id)
		return id
	}
	f.authorID = agent("che7090-author", f.claudeRuntime)
	f.sameLabID = agent("che7090-same", f.claudeRuntime)
	f.crossLabID = agent("che7090-cross", f.codexRuntime)
	f.issueID = dbfx.Issue(t, "design verdict", testutil.Cols{"status": "in_progress"})
	f.authorTask = f.task(t, f.authorID, f.claudeRuntime, "claude-test", "completed")

	// The guard is warn with the Design url property; the previous setting returns after the test.
	var prev []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&prev)
	f.setGuard(t, map[string]any{"mode": "block", "doop_origin": verdictDoopOrigin, "design_url_property_id": verdictDesignPropertyID})
	t.Cleanup(func() {
		// t.Context() is already cancelled when a cleanup runs, so the restore needs its own context.
		if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, string(prev)); err != nil {
			t.Errorf("restore workspace settings: %v", err)
		}
	})
	f.setDesignURL(t, verdictDoopOrigin+"/c/canvas-1?page=page-1&frame=frame-1")

	srv := httptest.NewServer(f.doop)
	t.Cleanup(srv.Close)
	previous := testHandler.DoopReader
	testHandler.DoopReader = doopread.New(doopread.Config{BaseURL: srv.URL, Token: verdictToken, AllowLoopbackHTTP: true})
	t.Cleanup(func() { testHandler.DoopReader = previous })
	f.serve(nil)
	return f
}

func (f *verdictFixture) setGuard(t *testing.T, guard any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"design_card_guard": guard})
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || $2::jsonb WHERE id = $1`, testWorkspaceID, string(raw))
}

func (f *verdictFixture) setDesignURL(t *testing.T, url string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET properties = CASE WHEN $2 = '' THEN '{}'::jsonb ELSE jsonb_build_object($3::text, $2::text) END WHERE id = $1`, f.issueID, url, verdictDesignPropertyID)
}

// task is a task of agentID on the issue, as the daemon claims it.
func (f *verdictFixture) task(t *testing.T, agentID, runtimeID, model, status string) string {
	t.Helper()
	return dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": f.issueID, "status": status, "claimed_model": model,
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
}

// serve sets the attestation view: a complete, current attestation of page-1 on
// canvas-1, bound to the issue, written by the author task; over changes keys.
func (f *verdictFixture) serve(over map[string]any) {
	view := map[string]any{
		"id": verdictAttestation, "canvasId": "canvas-1", "pageId": "page-1", "pageIssueId": f.issueID,
		"complete": true, "current": true, "ruleset": "ux-v1",
		"frames":             []map[string]any{{"frameId": "frame-1", "updatedAt": 1700000000}},
		"authors":            []map[string]any{{"kind": "agent", "agentId": f.authorID, "taskId": f.authorTask, "sources": []string{"write"}}},
		"provenanceComplete": true,
	}
	for k, v := range over {
		view[k] = v
	}
	f.doop.mu.Lock()
	f.doop.view, f.doop.status = view, 0
	f.doop.mu.Unlock()
}

func (f *verdictFixture) post(t *testing.T, agentID, taskID string, body any) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("POST", "/api/issues/"+f.issueID+"/design-verdicts", body), "id", f.issueID)
	if agentID != "" {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
	}
	return testutil.Call(t, testHandler.RecordDesignVerdict, req)
}

func (f *verdictFixture) rows(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'design_review_verdict'`, f.issueID)
}

func passBody() map[string]any {
	return map[string]any{"attestation_id": verdictAttestation, "verdict": "PASS"}
}

// reviewer is a running task of agentID on the issue.
func (f *verdictFixture) reviewer(t *testing.T, agentID, runtimeID, model string) string {
	t.Helper()
	return f.task(t, agentID, runtimeID, model, "running")
}

func (f *verdictFixture) wantCode(t *testing.T, resp *testutil.Response, status int, code string) {
	t.Helper()
	resp.Want(status)
	if got, _ := resp.Map()["code"].(string); got != code {
		t.Fatalf("code = %q (body %s), want %q", got, resp.Text(), code)
	}
	if n := f.rows(t); n != 0 {
		t.Fatalf("%d design_review_verdict rows after a refusal, want none", n)
	}
}

func TestDesignVerdict_IndependentPassIsRecorded(t *testing.T) {
	f := newVerdictFixture(t)
	task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	body := passBody()
	body["findings"] = []map[string]any{{"severity": "P3", "summary": "tighten spacing"}}
	resp := f.post(t, f.crossLabID, task, body).Want(http.StatusCreated)
	if resp.Map()["verdict"] != "PASS" {
		t.Fatalf("response = %s", resp.Text())
	}
	if n := f.rows(t); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
	var actorType, actorID, action string
	var details []byte
	dbfx.QueryRow(t, `SELECT actor_type, actor_id::text, action, details FROM activity_log WHERE issue_id = $1 AND action = 'design_review_verdict'`, f.issueID).Scan(&actorType, &actorID, &action, &details)
	if actorType != "agent" || actorID != f.crossLabID {
		t.Fatalf("actor = %s %s, want agent %s", actorType, actorID, f.crossLabID)
	}
	var d map[string]any
	if err := json.Unmarshal(details, &d); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"attestation_id": verdictAttestation, "verdict": "PASS", "canvas_id": "canvas-1", "page_id": "page-1", "ruleset": "ux-v1",
		"task_id": task, "runtime_provider": "codex", "claimed_model": "gpt-test",
	} {
		if d[key] != want {
			t.Errorf("details.%s = %v, want %v", key, d[key], want)
		}
	}
	for _, key := range []string{"findings", "frames", "author_agents", "author_labs"} {
		if _, ok := d[key]; !ok {
			t.Errorf("details lacks %s: %s", key, details)
		}
	}
	if labs, _ := d["author_labs"].([]any); len(labs) != 1 || labs[0] != "anthropic" {
		t.Errorf("author_labs = %v, want [anthropic]", d["author_labs"])
	}
}

// The author's task ended before the review: rows do not depend on time.
func TestDesignVerdict_RealIndependentPassAfterAuthorTaskEnded(t *testing.T) {
	f := newVerdictFixture(t)
	task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	f.post(t, f.crossLabID, task, passBody()).Want(http.StatusCreated)
}

func TestDesignVerdict_RunRequired(t *testing.T) {
	f := newVerdictFixture(t)
	running := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	done := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test", "completed")
	otherIssue := dbfx.Issue(t, "other", testutil.Cols{})
	elsewhere := dbfx.Task(t, f.crossLabID, testutil.Cols{"runtime_id": f.codexRuntime, "issue_id": otherIssue, "status": "running", "claimed_model": "gpt-test"})
	for name, c := range map[string]struct{ agent, task string }{
		"not a task token":     {"", ""},
		"task already ended":   {f.crossLabID, done},
		"task of other issue":  {f.crossLabID, elsewhere},
		"another agent's task": {f.sameLabID, running},
		"no task id":           {f.crossLabID, "not-a-uuid"},
	} {
		t.Run(name, func(t *testing.T) {
			f.wantCode(t, f.post(t, c.agent, c.task, passBody()), http.StatusForbidden, "design_verdict_run_required")
		})
	}
	// A dispatched task counts as a run, as for the workflow verdict.
	dispatched := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test", "dispatched")
	f.post(t, f.crossLabID, dispatched, passBody()).Want(http.StatusCreated)
}

func TestDesignVerdict_BadBody(t *testing.T) {
	f := newVerdictFixture(t)
	task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	for name, c := range map[string]struct {
		body any
		code string
	}{
		"no attestation":           {map[string]any{"verdict": "PASS"}, "design_verdict_bad_body"},
		"bad attestation id":       {map[string]any{"attestation_id": "../x", "verdict": "PASS"}, "design_verdict_bad_body"},
		"no verdict":               {map[string]any{"attestation_id": verdictAttestation}, "design_verdict_bad_body"},
		"lowercase verdict":        {map[string]any{"attestation_id": verdictAttestation, "verdict": "pass"}, "design_verdict_bad_body"},
		"unknown field":            {map[string]any{"attestation_id": verdictAttestation, "verdict": "PASS", "x": 1}, "design_verdict_bad_body"},
		"bad severity":             {map[string]any{"attestation_id": verdictAttestation, "verdict": "PASS", "findings": []map[string]any{{"severity": "P9", "summary": "x"}}}, "design_verdict_bad_body"},
		"empty summary":            {map[string]any{"attestation_id": verdictAttestation, "verdict": "PASS", "findings": []map[string]any{{"severity": "P2", "summary": " "}}}, "design_verdict_bad_body"},
		"changes with no findings": {map[string]any{"attestation_id": verdictAttestation, "verdict": "CHANGES"}, "design_verdict_findings_required"},
		"changes with only P2":     {map[string]any{"attestation_id": verdictAttestation, "verdict": "CHANGES", "findings": []map[string]any{{"severity": "P2", "summary": "x"}}}, "design_verdict_findings_required"},
	} {
		t.Run(name, func(t *testing.T) {
			f.wantCode(t, f.post(t, f.crossLabID, task, c.body), http.StatusBadRequest, c.code)
		})
	}
	req := withURLParam(newRequest("POST", "/api/issues/"+f.issueID+"/design-verdicts", nil), "id", f.issueID)
	req.Body = http.NoBody
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", f.crossLabID)
	req.Header.Set("X-Task-ID", task)
	f.wantCode(t, testutil.Call(t, testHandler.RecordDesignVerdict, req), http.StatusBadRequest, "design_verdict_bad_body")
}

func TestDesignVerdict_UnknownAttestationIs404(t *testing.T) {
	f := newVerdictFixture(t)
	task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	f.doop.mu.Lock()
	f.doop.status = http.StatusNotFound
	f.doop.mu.Unlock()
	f.wantCode(t, f.post(t, f.crossLabID, task, passBody()), http.StatusNotFound, designguard.CodeAttestationNotFound)
	// A CHANGES verdict names an attestation too.
	body := map[string]any{"attestation_id": verdictAttestation, "verdict": "CHANGES", "findings": []map[string]any{{"severity": "P1", "summary": "x"}}}
	f.wantCode(t, f.post(t, f.crossLabID, task, body), http.StatusNotFound, designguard.CodeAttestationNotFound)
}

// Conditions C2 to C5 and C4b, then the independence rules A1 to A5, through
// the fake doop. Each case is refused with 409 and writes no row.
func TestDesignVerdict_RefusalMatrix(t *testing.T) {
	type refusal struct {
		code     string
		design   *string        // issue Design url; nil keeps the default
		view     map[string]any // attestation view changes
		reviewer string         // "cross" (default), "author", "same"
		model    string         // reviewer claimed model; "" keeps the lab's
	}
	str := func(s string) *string { return &s }
	cases := map[string]refusal{
		"C2 Design url missing":         {code: designguard.CodeDesignURLMissing, design: str("")},
		"C2 Design url off origin":      {code: designguard.CodeDesignURLMissing, design: str("https://elsewhere.example.com/c/canvas-1?page=page-1")},
		"C2 Design url ambiguous":       {code: designguard.CodeDesignURLAmbiguous, design: str(verdictDoopOrigin + "/c/canvas-1?frame=a&frame=b")},
		"C3 canvas differs":             {code: designguard.CodeCanvasMismatch, view: map[string]any{"canvasId": "canvas-2"}},
		"C4 page differs":               {code: designguard.CodeFrameOutsidePage, view: map[string]any{"pageId": "page-2"}},
		"C4 frame not in frames":        {code: designguard.CodeFrameOutsidePage, view: map[string]any{"frames": []map[string]any{{"frameId": "frame-9", "updatedAt": 1}}}},
		"C4b page bound elsewhere":      {code: designguard.CodePageNotBound, view: map[string]any{"pageIssueId": "00000000-0000-0000-0000-000000000001"}},
		"C4b page bound to no issue":    {code: designguard.CodePageNotBound, view: map[string]any{"pageIssueId": nil}},
		"C5 attestation incomplete":     {code: designguard.CodeAttestationIncomplete, view: map[string]any{"complete": false}},
		"C5 attestation stale":          {code: designguard.CodeAttestationStale, view: map[string]any{"current": false}},
		"A1 provenance incomplete":      {code: designguard.CodeAuthorUnknown, view: map[string]any{"provenanceComplete": false}},
		"A1 human or legacy write":      {code: designguard.CodeAuthorUnknown, view: map[string]any{"authors": []map[string]any{{"kind": "unknown", "sources": []string{"legacy"}}}, "provenanceComplete": false}},
		"A1 task Multica does not know": {code: designguard.CodeAuthorUnknown, view: map[string]any{"authors": []map[string]any{{"kind": "agent", "agentId": "00000000-0000-0000-0000-0000000000aa", "taskId": "00000000-0000-0000-0000-0000000000bb"}}}},
		"A3 reviewer is an author":      {code: designguard.CodeSelfReview, reviewer: "author"},
		"A5 reviewer of the author lab": {code: designguard.CodeSameLab, reviewer: "same"},
		"A4 reviewer lab unlisted":      {code: designguard.CodeReviewerLabUnknown, model: "unlisted-model"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newVerdictFixture(t)
			if c.design != nil {
				f.setDesignURL(t, *c.design)
			}
			f.serve(c.view)
			agent, runtime, model := f.crossLabID, f.codexRuntime, "gpt-test"
			switch c.reviewer {
			case "author":
				agent, runtime, model = f.authorID, f.claudeRuntime, "claude-test"
			case "same":
				agent, runtime, model = f.sameLabID, f.claudeRuntime, "claude-test"
			}
			if c.model != "" {
				model = c.model
			}
			task := f.reviewer(t, agent, runtime, model)
			f.wantCode(t, f.post(t, agent, task, passBody()), http.StatusConflict, c.code)
		})
	}
}

// Review r2 cases at write: the ledger rows decide, never timing.
func TestDesignVerdict_ReviewR2Cases(t *testing.T) {
	t.Run("another lab made only final metadata edits, the first author reviews", func(t *testing.T) {
		f := newVerdictFixture(t)
		editor := f.task(t, f.crossLabID, f.codexRuntime, "gpt-test", "completed")
		f.serve(map[string]any{"authors": []map[string]any{
			{"kind": "agent", "agentId": f.authorID, "taskId": f.authorTask},
			{"kind": "agent", "agentId": f.crossLabID, "taskId": editor},
		}})
		task := f.reviewer(t, f.authorID, f.claudeRuntime, "claude-test")
		f.wantCode(t, f.post(t, f.authorID, task, passBody()), http.StatusConflict, designguard.CodeSelfReview)
	})
	t.Run("a copied page inherits the author rows, another agent of the author lab reviews", func(t *testing.T) {
		f := newVerdictFixture(t)
		task := f.reviewer(t, f.sameLabID, f.claudeRuntime, "claude-test")
		f.wantCode(t, f.post(t, f.sameLabID, task, passBody()), http.StatusConflict, designguard.CodeSameLab)
	})
	t.Run("the reviewer edited a frame in its own run", func(t *testing.T) {
		f := newVerdictFixture(t)
		task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
		f.serve(map[string]any{"authors": []map[string]any{
			{"kind": "agent", "agentId": f.authorID, "taskId": f.authorTask},
			{"kind": "agent", "agentId": f.crossLabID, "taskId": task},
		}})
		f.wantCode(t, f.post(t, f.crossLabID, task, passBody()), http.StatusConflict, designguard.CodeSelfReview)
	})
	t.Run("user rows are people and do not enter A3 to A5", func(t *testing.T) {
		f := newVerdictFixture(t)
		f.serve(map[string]any{"authors": []map[string]any{
			{"kind": "agent", "agentId": f.authorID, "taskId": f.authorTask},
			{"kind": "user", "userId": "u-1"},
		}})
		task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
		f.post(t, f.crossLabID, task, passBody()).Want(http.StatusCreated)
	})
}

// A CHANGES verdict needs only a P0 or P1 finding: C2 to C5 and A1 to A5 are for PASS.
func TestDesignVerdict_ChangesSkipsPassChecks(t *testing.T) {
	f := newVerdictFixture(t)
	f.serve(map[string]any{"current": false, "provenanceComplete": false})
	task := f.reviewer(t, f.sameLabID, f.claudeRuntime, "claude-test")
	body := map[string]any{"attestation_id": verdictAttestation, "verdict": "CHANGES", "findings": []map[string]any{{"severity": "P1", "summary": "contrast fails"}}}
	f.post(t, f.sameLabID, task, body).Want(http.StatusCreated)
	if n := f.rows(t); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestDesignVerdict_DoopUnavailableIs503(t *testing.T) {
	f := newVerdictFixture(t)
	task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	f.doop.mu.Lock()
	f.doop.status = http.StatusInternalServerError
	f.doop.mu.Unlock()
	f.wantCode(t, f.post(t, f.crossLabID, task, passBody()), http.StatusServiceUnavailable, designguard.CodeDoopUnavailable)

	testHandler.DoopReader = nil
	f.wantCode(t, f.post(t, f.crossLabID, task, passBody()), http.StatusServiceUnavailable, designguard.CodeDoopUnavailable)
}

func TestDesignVerdict_MisconfiguredGuardIs503(t *testing.T) {
	for name, guard := range map[string]any{
		"mode off":           map[string]any{"mode": "off"},
		"no mode and origin": map[string]any{"mode": "block"},
		"not an object":      "block",
	} {
		t.Run(name, func(t *testing.T) {
			f := newVerdictFixture(t)
			task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
			f.setGuard(t, guard)
			f.wantCode(t, f.post(t, f.crossLabID, task, passBody()), http.StatusServiceUnavailable, designguard.CodeGuardMisconfigured)
		})
	}
}

// The newest verdict per issue and attestation is the one C6 reads.
func TestDesignVerdict_NewestVerdictPerAttestation(t *testing.T) {
	f := newVerdictFixture(t)
	task := f.reviewer(t, f.crossLabID, f.codexRuntime, "gpt-test")
	changes := map[string]any{"attestation_id": verdictAttestation, "verdict": "CHANGES", "findings": []map[string]any{{"severity": "P0", "summary": "unreadable"}}}
	newest := func(attestation string) string {
		row, err := testHandler.Queries.GetNewestDesignVerdict(t.Context(), newestParams(t, f.issueID, attestation))
		if err != nil {
			return ""
		}
		var d struct {
			Verdict string `json:"verdict"`
		}
		json.Unmarshal(row.Details, &d)
		return d.Verdict
	}
	if got := newest(verdictAttestation); got != "" {
		t.Fatalf("newest before any verdict = %q, want none", got)
	}
	f.post(t, f.crossLabID, task, passBody()).Want(http.StatusCreated)
	if got := newest(verdictAttestation); got != "PASS" {
		t.Fatalf("newest = %q, want PASS", got)
	}
	f.post(t, f.crossLabID, task, changes).Want(http.StatusCreated)
	if got := newest(verdictAttestation); got != "CHANGES" {
		t.Fatalf("newest after CHANGES = %q, want CHANGES", got)
	}
	if got := newest("run-2"); got != "" {
		t.Fatalf("another attestation sees %q, want none", got)
	}
}

func newestParams(t *testing.T, issueID, attestation string) db.GetNewestDesignVerdictParams {
	t.Helper()
	return db.GetNewestDesignVerdictParams{IssueID: parseUUID(issueID), AttestationID: attestation}
}
