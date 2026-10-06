package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const wdNoRoot = "00000000-0000-4000-8000-00000000f00d"

type wdKit struct {
	t         *testing.T
	projectID string
	issueID   string
}

// newWakeupDefinitionKit opens the activation gate, which is closed unless a
// test asks, and removes every definition the test wrote.
func newWakeupDefinitionKit(t *testing.T) *wdKit {
	t.Helper()
	withFeatureFlag(t, testHandler, featureflags.WakeupDefinitionWrites, true)
	k := &wdKit{t: t, projectID: dbfx.Project(t, "wd project")}
	k.issueID = dbfx.Issue(t, "wd issue", testutil.Cols{"project_id": k.projectID, "number": nextWorkspaceIssueNumber(t)})
	wipeWakeupDefinitions(t)
	t.Cleanup(func() { wipeWakeupDefinitions(t) })
	return k
}

func wipeWakeupDefinitions(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `DELETE FROM issue_wakeup_definition WHERE workspace_id = $1`, testWorkspaceID)
}

func wdCount(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id = $1`, testWorkspaceID)
}

// wdScope is one scope's route set and the URL params its handlers read.
type wdScope struct {
	t      *testing.T
	kind   service.WakeupScope
	params []string
}

func (k *wdKit) workspace() wdScope {
	return wdScope{t: k.t, kind: service.WakeupScopeWorkspace}
}
func (k *wdKit) project() wdScope {
	return wdScope{t: k.t, kind: service.WakeupScopeProject, params: []string{"id", k.projectID}}
}
func (k *wdKit) issue() wdScope {
	return wdScope{t: k.t, kind: service.WakeupScopeIssue, params: []string{"id", k.issueID}}
}

func (s wdScope) api() wakeupDefinitionAPI { return testHandler.WakeupDefinitionAPI(s.kind) }

type wdAs func(*http.Request) *http.Request

func (s wdScope) call(h http.HandlerFunc, method, query string, body any, as []wdAs, kv ...string) *testutil.Response {
	s.t.Helper()
	req := withURLParams(newRequest(method, "/wakeup-definitions"+query, body), append(append([]string{}, s.params...), kv...)...)
	for _, f := range as {
		req = f(req)
	}
	return testutil.Call(s.t, h, req)
}

func wdBody(revision any, config map[string]any) map[string]any {
	return map[string]any{"revision": revision, "config": config}
}

func (s wdScope) put(rule string, revision any, config map[string]any, as ...wdAs) *testutil.Response {
	s.t.Helper()
	return s.call(s.api().Put, "PUT", "", wdBody(revision, config), as, "rule", rule)
}

func (s wdScope) create(config map[string]any, as ...wdAs) *testutil.Response {
	s.t.Helper()
	return s.call(s.api().Create, "POST", "", wdBody(0, config), as)
}

func (s wdScope) del(rule, revision string, as ...wdAs) *testutil.Response {
	s.t.Helper()
	return s.call(s.api().Delete, "DELETE", "?revision="+revision, nil, as, "rule", rule)
}

func (s wdScope) list(as ...wdAs) wakeupDefinitionListResponse {
	s.t.Helper()
	var out wakeupDefinitionListResponse
	s.call(s.api().List, "GET", "", nil, as).Want(http.StatusOK).JSON(&out)
	return out
}

func (s wdScope) effective(rule string, as ...wdAs) wakeupEffectiveResponse {
	s.t.Helper()
	var out wakeupEffectiveResponse
	s.call(s.api().Effective, "GET", "", nil, as, "rule", rule).Want(http.StatusOK).JSON(&out)
	return out
}

func (s wdScope) preview(rule string, config map[string]any, as ...wdAs) *testutil.Response {
	s.t.Helper()
	return s.call(s.api().Preview, "POST", "", map[string]any{"rule_key": rule, "config": config}, as)
}

func cfg(fields map[string]any) map[string]any {
	out := map[string]any{"v": 1}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func asUser(userID string) wdAs {
	return func(r *http.Request) *http.Request { r.Header.Set("X-User-ID", userID); return r }
}

// asAgentRun speaks as an agent from inside one of its live runs, which the
// owner's session header still backs: the actor is the agent.
func (k *wdKit) asAgentRun() wdAs {
	agentID := dbfx.Agent(k.t, "wd actor agent", handlerTestRuntimeID(k.t))
	taskID := dbfx.Task(k.t, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(k.t), "originator_user_id": testUserID, "accountable_user_id": testUserID})
	return func(r *http.Request) *http.Request { return asRun(r, agentID, taskID) }
}

func wdMember(t *testing.T, name, role string) string {
	t.Helper()
	id := createWorkspaceMemberUser(t, name, strings.ReplaceAll(strings.ToLower(name), " ", "-")+"@wd.multica.test")
	if role != "member" {
		dbfx.Exec(t, `UPDATE member SET role = $1 WHERE workspace_id = $2 AND user_id = $3`, role, testWorkspaceID, id)
	}
	return id
}

func decodeDefinition(t *testing.T, resp *testutil.Response) wakeupDefinitionResponse {
	t.Helper()
	var out wakeupDefinitionResponse
	resp.JSON(&out)
	return out
}

func configKeys(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("config %s: %v", raw, err)
	}
	return out
}

// ---- activation gate -------------------------------------------------------

func TestWakeupDefinitionWritesClosedByDefault(t *testing.T) {
	k := &wdKit{t: t, projectID: dbfx.Project(t, "wd project")}
	k.issueID = dbfx.Issue(t, "wd issue", testutil.Cols{"project_id": k.projectID, "number": nextWorkspaceIssueNumber(t)})
	wipeWakeupDefinitions(t)
	t.Cleanup(func() { wipeWakeupDefinitions(t) })
	for name, s := range map[string]wdScope{"workspace": k.workspace(), "project": k.project(), "issue": k.issue()} {
		t.Run(name, func(t *testing.T) {
			s.t = t
			for _, resp := range []*testutil.Response{
				s.put("child_done", 0, cfg(map[string]any{"name": "x"})),
				s.del("child_done", "1"),
			} {
				resp.Want(http.StatusForbidden)
				if got := resp.Map()["code"]; got != "wakeup_definition_writes_closed" {
					t.Fatalf("code = %v: %s", got, resp.Text())
				}
			}
			// Reads and previews persist nothing, so the closed gate leaves them open.
			if caps := s.list().Capabilities; caps.DefinitionWrites {
				t.Fatal("capabilities report writes open")
			}
			s.preview("child_done", cfg(map[string]any{"name": "x"})).Want(http.StatusOK)
		})
	}
	k.workspace().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"})).Want(http.StatusForbidden)
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d definitions persisted behind a closed gate", n)
	}
}

// ---- authorization ---------------------------------------------------------

func TestWakeupDefinitionAccessMatrix(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	admin := wdMember(t, "WD Admin", "admin")
	member := wdMember(t, "WD Member", "member")
	outsider := dbfx.User(t, "WD Outsider", "wd-outsider@multica.test")
	agentRun := k.asAgentRun()
	actors := []struct {
		name string
		as   wdAs
		// expected status per scope: workspace, project, issue
		want [3]int
	}{
		{"owner", asUser(testUserID), [3]int{200, 200, 200}},
		{"admin", asUser(admin), [3]int{200, 200, 200}},
		{"member", asUser(member), [3]int{403, 403, 200}},
		{"outsider", asUser(outsider), [3]int{403, 403, 403}},
		{"agent", agentRun, [3]int{403, 403, 403}},
	}
	scopes := []wdScope{k.workspace(), k.project(), k.issue()}
	for _, a := range actors {
		for i, s := range scopes {
			wipeWakeupDefinitions(t)
			resp := s.put("child_done", 0, cfg(map[string]any{"name": "label"}), a.as)
			if resp.Code != a.want[i] {
				t.Errorf("%s writes %s scope: got %d, want %d: %s", a.name, s.kind, resp.Code, a.want[i], resp.Text())
			}
			if want := map[bool]int{true: 1, false: 0}[a.want[i] == 200]; wdCount(t) != want {
				t.Errorf("%s writes %s scope: %d definitions persisted, want %d", a.name, s.kind, wdCount(t), want)
			}
		}
	}
	// An agent can read what it may see but cannot create a root default.
	k.workspace().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"}), agentRun).Want(http.StatusForbidden)
	k.project().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"}), asUser(member)).Want(http.StatusForbidden)
	k.workspace().list(agentRun)
}

func TestWakeupDefinitionDeniesForeignWorkspaceReferences(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	foreignWS := dbfx.Insert(t, "workspace", testutil.Cols{"name": "WD Foreign", "slug": "wd-foreign", "description": "", "issue_prefix": "WDF"})
	foreignRuntime := dbfx.Insert(t, "agent_runtime", testutil.Cols{
		"workspace_id": foreignWS, "daemon_id": nil, "name": "wd foreign runtime", "runtime_mode": "cloud", "provider": "handler_test_runtime",
		"status": "online", "device_info": "", "metadata": testutil.Raw("'{}'::jsonb"), "last_seen_at": testutil.Raw("now()"),
	})
	foreignAgent := dbfx.Agent(t, "wd foreign agent", foreignRuntime, testutil.Cols{"workspace_id": foreignWS, "permission_mode": "public_to"})
	foreignSquad := dbfx.Squad(t, "wd foreign squad", foreignAgent, testutil.Cols{"workspace_id": foreignWS})
	foreignLabel := dbfx.Insert(t, "issue_label", testutil.Cols{"workspace_id": foreignWS, "resource_type": "issue", "name": "wd foreign", "description": "", "color": "#fff"})
	foreignProject := dbfx.Project(t, "wd foreign project", testutil.Cols{"workspace_id": foreignWS})
	foreignIssue := dbfx.Issue(t, "wd foreign issue", testutil.Cols{"workspace_id": foreignWS, "number": 9001})

	for name, config := range map[string]map[string]any{
		"agent target": cfg(map[string]any{"target": map[string]any{"type": "agent", "id": foreignAgent}}),
		"squad target": cfg(map[string]any{"target": map[string]any{"type": "squad", "id": foreignSquad}}),
		"label filter": cfg(map[string]any{"filters": map[string]any{"labels": []string{foreignLabel}}}),
	} {
		for _, s := range []wdScope{k.workspace(), k.project(), k.issue()} {
			if resp := s.put("child_done", 0, config); resp.Code != http.StatusForbidden {
				t.Errorf("%s at %s scope: got %d, want 403: %s", name, s.kind, resp.Code, resp.Text())
			}
			// A preview must not confirm a foreign reference either.
			if resp := s.preview("child_done", config); resp.Code != http.StatusForbidden {
				t.Errorf("preview %s at %s scope: got %d, want 403: %s", name, s.kind, resp.Code, resp.Text())
			}
		}
	}
	projectScope := wdScope{t: t, kind: service.WakeupScopeProject, params: []string{"id", foreignProject}}
	projectScope.put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusNotFound)
	projectScope.call(projectScope.api().List, "GET", "", nil, nil).Want(http.StatusNotFound)
	issueScope := wdScope{t: t, kind: service.WakeupScopeIssue, params: []string{"id", foreignIssue}}
	issueScope.put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusNotFound)
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d definitions persisted from denied writes", n)
	}
	// Same-workspace references pass.
	own := handlerTestAgentID(t)
	k.project().put("child_done", 0, cfg(map[string]any{"target": map[string]any{"type": "agent", "id": own}})).Want(http.StatusOK)
}

func TestWakeupDefinitionTargetNeedsInvocationPermission(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Invoker", "member")
	// A private agent owned by the owner: the member cannot invoke it, and
	// neither does the owner's admin role help another admin (no admin bypass).
	private := dbfx.Agent(t, "wd private agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": testUserID})
	memberOwned := dbfx.Agent(t, "wd member agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": member})
	admin := wdMember(t, "WD Other Admin", "admin")
	archived := dbfx.Agent(t, "wd archived agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": testUserID, "archived_at": testutil.Raw("now()")})
	target := func(id string) map[string]any {
		return cfg(map[string]any{"target": map[string]any{"type": "agent", "id": id}})
	}
	k.issue().put("child_done", 0, target(private), asUser(member)).Want(http.StatusForbidden)
	k.project().put("child_done", 0, target(private), asUser(admin)).Want(http.StatusForbidden)
	k.issue().put("child_done", 0, target(archived)).Want(http.StatusForbidden)
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d definitions persisted from denied writes", n)
	}
	k.issue().put("child_done", 0, target(memberOwned), asUser(member)).Want(http.StatusOK)
	k.workspace().put("pr_merged", 0, target(private)).Want(http.StatusOK)
	// A squad is invoked through its leader.
	squad := dbfx.Squad(t, "wd squad", private)
	k.project().put("pr_merged", 0, cfg(map[string]any{"target": map[string]any{"type": "squad", "id": squad}}), asUser(admin)).Want(http.StatusForbidden)
	k.project().put("pr_merged", 0, cfg(map[string]any{"target": map[string]any{"type": "squad", "id": squad}})).Want(http.StatusOK)
}

// ---- revisions and preview -------------------------------------------------

func TestWakeupDefinitionRevisionConflict(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	s := k.project()
	first := decodeDefinition(t, s.put("child_done", 0, cfg(map[string]any{"name": "one"})).Want(http.StatusOK))
	if first.Revision != 1 || first.Scope != "project" || first.ScopeID != k.projectID || first.Root {
		t.Fatalf("first write: %+v", first)
	}
	s.put("child_done", 0, cfg(map[string]any{"name": "again"})).Want(http.StatusConflict)
	second := decodeDefinition(t, s.put("child_done", 1, cfg(map[string]any{"name": "two"})).Want(http.StatusOK))
	if second.Revision != 2 {
		t.Fatalf("second write: %+v", second)
	}
	s.put("child_done", 1, cfg(map[string]any{"name": "stale"})).Want(http.StatusConflict)
	s.del("child_done", "1").Want(http.StatusConflict)
	s.del("child_done", "").Want(http.StatusBadRequest)
	if got := s.list().Definitions; len(got) != 1 || got[0].Revision != 2 || !strings.Contains(string(got[0].Config), `"two"`) {
		t.Fatalf("stale writes changed the definition: %+v", got)
	}
	s.del("child_done", "2").Want(http.StatusNoContent)
	s.del("child_done", "2").Want(http.StatusNotFound)
	// A write that expects an existing definition cannot create one.
	s.put("child_done", 5, cfg(map[string]any{"name": "ghost"})).Want(http.StatusConflict)
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d definitions left", n)
	}
	// A workspace built-in is disabled, never deleted.
	k.workspace().put("child_done", 0, cfg(map[string]any{"name": "ws"})).Want(http.StatusOK)
	k.workspace().del("child_done", "1").Want(http.StatusBadRequest)
}

// Writers observing the same revision race for it: exactly one wins, the rest
// conflict, and nothing is stored twice.
func TestWakeupDefinitionConcurrentWritersConflict(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	const writers = 8
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = k.project().put("child_done", 0, cfg(map[string]any{"name": "writer " + strconv.Itoa(i)})).Code
		}()
	}
	wg.Wait()
	won, conflicted := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			won++
		case http.StatusConflict:
			conflicted++
		}
	}
	if won != 1 || conflicted != writers-1 || wdCount(t) != 1 {
		t.Fatalf("codes = %v, rows = %d; want one winner and %d conflicts", codes, wdCount(t), writers-1)
	}
}

func TestWakeupDefinitionPreviewDoesNotPersist(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settingsBefore string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settingsBefore)
	// Resolved against what is stored: the project sets a name, the issue
	// previews a different instruction and disables the rule.
	k.project().put("child_done", 0, cfg(map[string]any{"name": "project label"})).Want(http.StatusOK)
	before := wdCount(t)

	var eff wakeupEffectiveResponse
	k.issue().preview("child_done", cfg(map[string]any{"enabled": false, "name": "issue label", "instruction": "previewed"})).Want(http.StatusOK).JSON(&eff)
	if eff.Enabled || eff.Sources["instruction"] != "issue" || eff.Sources["name"] != "issue" || !eff.Applicable {
		t.Fatalf("preview: %+v", eff)
	}
	// enabled and name override inherited values; instruction overrides nothing.
	if got := strings.Join(eff.Overrides, ","); got != "enabled,name" {
		t.Fatalf("overrides = %q", got)
	}
	// Workspace-scope alias fields are previewed without touching settings.
	k.workspace().preview("child_done", cfg(map[string]any{"enabled": false, "instruction": "ws previewed"})).Want(http.StatusOK)
	// A preview of a new custom root names no key and persists nothing.
	k.workspace().preview("", cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "new"})).Want(http.StatusOK)
	// An invalid proposal fails exactly like a write would.
	k.workspace().preview("", cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}})).Want(http.StatusBadRequest)

	if got := wdCount(t); got != before {
		t.Fatalf("preview persisted: %d -> %d definitions", before, got)
	}
	var settingsAfter string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settingsAfter)
	if settingsAfter != settingsBefore {
		t.Fatalf("preview changed workspace settings:\n%s\n%s", settingsBefore, settingsAfter)
	}
}

// ---- schema ----------------------------------------------------------------

func TestWakeupDefinitionRejectsUnimplementedAndInvalidConfig(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	prTrigger := map[string]any{"kind": "pr_merged"}
	cases := map[string]map[string]any{
		"empty":                    cfg(nil),
		"event trigger":            cfg(map[string]any{"trigger": map[string]any{"kind": "event"}, "instruction": "x"}),
		"cron trigger":             cfg(map[string]any{"trigger": map[string]any{"kind": "cron"}, "instruction": "x"}),
		"trigger kind of built-in": cfg(map[string]any{"trigger": prTrigger}),
		"aggregate limit zero":     cfg(map[string]any{"aggregate_limit": 0}),
		"aggregate limit too high": cfg(map[string]any{"aggregate_limit": 1001}),
		"unknown active run":       cfg(map[string]any{"active_run": "queue"}),
		"schedule":                 cfg(map[string]any{"schedule": map[string]any{"every_seconds": 3600}}),
		"unknown field":            cfg(map[string]any{"surprise": true}),
		"future version":           {"v": 2, "name": "x"},
		"instruction too long":     cfg(map[string]any{"instruction": strings.Repeat("x", 12001)}),
		"blank instruction":        cfg(map[string]any{"instruction": "   "}),
		"max fires zero":           cfg(map[string]any{"max_fires": 0}),
		"max fires too many":       cfg(map[string]any{"max_fires": 1001}),
		"rate limit too high":      cfg(map[string]any{"rate_limit": 13}),
		"bad mode":                 cfg(map[string]any{"mode": "forever"}),
		"bad target type":          cfg(map[string]any{"target": map[string]any{"type": "member"}}),
		"assignee with id":         cfg(map[string]any{"target": map[string]any{"type": "assignee", "id": handlerTestAgentID(t)}}),
		"agent without id":         cfg(map[string]any{"target": map[string]any{"type": "agent"}}),
		"expiry both":              cfg(map[string]any{"expiry": map[string]any{"at": "2099-01-01T00:00:00Z", "after_seconds": 3600}}),
		"expiry too soon":          cfg(map[string]any{"expiry": map[string]any{"after_seconds": 5}}),
		"expiry in the past":       cfg(map[string]any{"expiry": map[string]any{"at": "2001-01-01T00:00:00Z"}}),
		"bad priority":             cfg(map[string]any{"filters": map[string]any{"priorities": []string{"asap"}}}),
		"branch filter on child":   cfg(map[string]any{"filters": map[string]any{"base_branch": "main"}}),
		"ci filter on pr_merged":   cfg(map[string]any{"filters": map[string]any{"ci": "error"}}),
	}
	for name, config := range cases {
		rule := "child_done"
		if name == "ci filter on pr_merged" {
			rule = "pr_merged"
		}
		if resp := k.project().put(rule, 0, config); resp.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", name, resp.Code, resp.Text())
		}
	}
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d invalid definitions persisted", n)
	}
	// Valid controls of the implemented kinds are accepted.
	k.project().put("pr_checks_failed", 0, cfg(map[string]any{
		"mode": "continuous", "max_fires": 5, "rate_limit": 6,
		"expiry":  map[string]any{"after_seconds": 3600},
		"filters": map[string]any{"head_branch": "main", "ci": "failure", "priorities": []string{"urgent", "high"}},
	})).Want(http.StatusOK)
	// The failing-checks event carries no base branch, so that filter is refused.
	k.project().put("pr_checks_failed", 1, cfg(map[string]any{"filters": map[string]any{"base_branch": "main"}})).Want(http.StatusBadRequest)
	// An unknown rule key is not a rule.
	k.project().put("not-a-rule", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusBadRequest)
}

// An aggregate cap is a workspace or project setting: an explicit value is
// stored as sent, an override that does not mention it gains none, and an issue
// cannot hold one.
func TestWakeupDefinitionAggregateLimitScopes(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	ws := decodeDefinition(t, k.workspace().put("pr_merged", 0, cfg(map[string]any{"aggregate_limit": 6})).Want(http.StatusOK))
	if got := string(configKeys(t, ws.Config)["aggregate_limit"]); got != "6" {
		t.Fatalf("workspace cap stored as %q: %s", got, ws.Config)
	}
	proj := decodeDefinition(t, k.project().put("pr_merged", 0, cfg(map[string]any{"instruction": "Project text."})).Want(http.StatusOK))
	if _, ok := configKeys(t, proj.Config)["aggregate_limit"]; ok {
		t.Fatalf("an instruction-only override gained a cap: %s", proj.Config)
	}
	k.project().put("pr_merged", proj.Revision, cfg(map[string]any{"aggregate_limit": 3})).Want(http.StatusOK)
	k.issue().put("pr_merged", 0, cfg(map[string]any{"aggregate_limit": 3})).Want(http.StatusBadRequest)
	explicit := decodeDefinition(t, k.workspace().create(cfg(map[string]any{
		"enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "Summarise.", "aggregate_limit": 5,
	})).Want(http.StatusCreated))
	if got := string(configKeys(t, explicit.Config)["aggregate_limit"]); got != "5" {
		t.Fatalf("an explicit cap on a new root is %q, want 5", got)
	}
}

func TestWakeupDefinitionCustomRuleRootAndOverrides(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	root := decodeDefinition(t, k.workspace().create(cfg(map[string]any{
		"name": "PR watcher", "enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "Summarise the merge.",
	})).Want(http.StatusCreated))
	if !root.Root || root.Revision != 1 || len(root.RuleKey) != 36 {
		t.Fatalf("root: %+v", root)
	}
	// Creation inserts the 12/hour aggregate cap and nothing else: no fire cap,
	// mode or defer.
	keys := configKeys(t, root.Config)
	if got := string(keys["aggregate_limit"]); got != "12" {
		t.Errorf("a new custom root has aggregate_limit %q, want the default 12: %s", got, root.Config)
	}
	for _, banned := range []string{"max_fires", "mode", "active_run", "rate_limit", "expiry"} {
		if _, ok := keys[banned]; ok {
			t.Errorf("creation inserted a default %s: %s", banned, root.Config)
		}
	}
	// A project root and an override of another scope's root.
	k.project().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_checks_failed"}, "instruction": "Fix CI."})).Want(http.StatusCreated)
	k.project().put(root.RuleKey, 0, cfg(map[string]any{"enabled": false})).Want(http.StatusOK)
	k.issue().put(root.RuleKey, 0, cfg(map[string]any{"instruction": "Issue text."})).Want(http.StatusOK)
	eff := k.issue().effective(root.RuleKey)
	if !eff.Applicable || eff.Enabled || eff.Sources["instruction"] != "issue" || eff.Sources["trigger"] != "workspace" || eff.Sources["enabled"] != "project" {
		t.Fatalf("effective: %+v", eff)
	}
	// An override with no root in its chain is refused, as is a second root via PUT.
	k.project().put(wdNoRoot, 0, cfg(map[string]any{"enabled": true})).Want(http.StatusBadRequest)
	k.workspace().put(wdNoRoot, 0, cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"})).Want(http.StatusBadRequest)
	// An override cannot strip what a custom rule needs to be runnable.
	k.issue().put(root.RuleKey, 1, cfg(map[string]any{"trigger": nil})).Want(http.StatusBadRequest)
	// Retiring the root retires the rule below it: the override stays stored
	// but is reported inapplicable and never promoted.
	k.workspace().del(root.RuleKey, "1").Want(http.StatusNoContent)
	retired := k.issue().effective(root.RuleKey)
	if retired.Applicable || retired.InapplicableReason != "no_root" || retired.Enabled {
		t.Fatalf("retired rule: %+v", retired)
	}
}

func TestWakeupDefinitionScopeCeiling(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	for i := range 32 {
		k.project().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "rule " + strconv.Itoa(i)})).Want(http.StatusCreated)
	}
	k.project().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "one too many"})).Want(http.StatusBadRequest)
	// Another scope has its own ceiling, and an existing definition still updates.
	k.workspace().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "ok"})).Want(http.StatusCreated)
	first := k.project().list().Definitions[0]
	k.project().put(first.RuleKey, first.Revision, cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "edited"})).Want(http.StatusOK)
}

// ---- legacy aliases --------------------------------------------------------

func TestWakeupDefinitionWorkspaceAliasesStayInSettings(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})

	// enabled and the child_done instruction are the settings aliases: the write
	// lands there, and the definition store keeps no second copy.
	saved := decodeDefinition(t, k.workspace().put("child_done", 0, cfg(map[string]any{"enabled": false, "instruction": "Alias text."})).Want(http.StatusOK))
	var got struct {
		Enabled     *bool   `json:"system_wakeup_child_done"`
		Instruction *string `json:"system_wakeup_child_done_instruction"`
	}
	var raw string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw)
	if err := json.Unmarshal([]byte(raw), &got); err != nil || got.Enabled == nil || *got.Enabled || got.Instruction == nil || *got.Instruction != "Alias text." {
		t.Fatalf("settings aliases not written: %s", raw)
	}
	if n := wdCount(t); n != 0 {
		t.Fatalf("alias fields stored a second copy (%d rows)", n)
	}
	if keys := configKeys(t, saved.Config); string(keys["enabled"]) != "false" || string(keys["instruction"]) != `"Alias text."` || saved.Revision == 0 {
		t.Fatalf("response must show the effective alias values: %+v", saved)
	}
	// The observed revision includes the alias state, so a stale write conflicts.
	k.workspace().put("child_done", 0, cfg(map[string]any{"enabled": true})).Want(http.StatusConflict)
	// Non-alias fields still go to the store, next to the aliases.
	next := decodeDefinition(t, k.workspace().put("child_done", saved.Revision, cfg(map[string]any{"enabled": true, "name": "Label"})).Want(http.StatusOK))
	if n := wdCount(t); n != 1 {
		t.Fatalf("definition rows = %d, want 1", n)
	}
	var stored string
	dbfx.QueryRow(t, `SELECT config::text FROM issue_wakeup_definition WHERE workspace_id = $1`, testWorkspaceID).Scan(&stored)
	if strings.Contains(stored, "enabled") || strings.Contains(stored, "instruction") || !strings.Contains(stored, "Label") {
		t.Fatalf("stored config = %s", stored)
	}
	if list := k.workspace().list().Definitions; len(list) != 1 || list[0].Revision != next.Revision {
		t.Fatalf("list must show the same revision a write expects: %+v vs %+v", list, next)
	}
	// The PR rules' enabled alias is a settings key as well.
	k.workspace().put("pr_merged", 0, cfg(map[string]any{"enabled": false})).Want(http.StatusOK)
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw)
	if !strings.Contains(raw, `"github_wake_on_pr_merge": false`) {
		t.Fatalf("pr alias not written: %s", raw)
	}
}

// ---- visibility ------------------------------------------------------------

func TestWakeupDefinitionRedactsTargetsTheViewerCannotSee(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Viewer", "member")
	private := dbfx.Agent(t, "wd hidden agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": testUserID})
	k.workspace().put("pr_merged", 0, cfg(map[string]any{"name": "Hidden", "instruction": "secret prompt", "target": map[string]any{"type": "agent", "id": private}})).Want(http.StatusOK)

	owner := k.workspace().list().Definitions
	if len(owner) == 0 || owner[0].Redacted || !strings.Contains(string(owner[0].Config), "secret prompt") {
		t.Fatalf("owner view: %+v", owner)
	}
	for _, view := range k.workspace().list(asUser(member)).Definitions {
		if !view.Redacted || strings.Contains(string(view.Config), "secret prompt") || strings.Contains(string(view.Config), private) {
			t.Fatalf("member view leaks the private target: %+v", view)
		}
	}
	eff := k.issue().effective("pr_merged", asUser(member))
	if !eff.Redacted || strings.Contains(string(eff.Config), "secret prompt") || strings.Contains(string(eff.Config), private) {
		t.Fatalf("effective view leaks the private target: %+v", eff)
	}
	if full := k.issue().effective("pr_merged"); full.Redacted || full.Sources["target"] != "workspace" {
		t.Fatalf("owner effective view: %+v", full)
	}
	// A preview by the member cannot probe the hidden agent either.
	k.issue().preview("pr_merged", cfg(map[string]any{"target": map[string]any{"type": "agent", "id": private}}), asUser(member)).Want(http.StatusForbidden)
}

// ---- correction round (Sol P1/P2, OCR high) --------------------------------

// Alias revisions are 62-bit hashes. A JSON number loses them in JavaScript, so
// the wire carries a decimal string and a write echoes it back unchanged.
func TestWakeupDefinitionRevisionIsLosslessOnTheWire(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})

	var saved map[string]any
	k.workspace().put("child_done", 0, cfg(map[string]any{"enabled": false, "instruction": "Alias text."})).Want(http.StatusOK).JSON(&saved)
	rev, ok := saved["revision"].(string)
	if !ok {
		t.Fatalf("revision must be a JSON string, got %T %v", saved["revision"], saved["revision"])
	}
	if n, err := strconv.ParseInt(rev, 10, 64); err != nil || n <= 1<<53 {
		t.Fatalf("alias revision %q is not beyond the safe integer range; the test would prove nothing", rev)
	}
	// The list shows the same string, and echoing it back as a string updates.
	if list := k.workspace().list().Definitions; len(list) != 1 || strconv.FormatInt(int64(list[0].Revision), 10) != rev {
		t.Fatalf("list revision differs from the write's %s: %+v", rev, list)
	}
	k.workspace().put("child_done", rev, cfg(map[string]any{"enabled": true, "name": "Label"})).Want(http.StatusOK)
	// A rounded number (what a double-based client would send) conflicts.
	n, _ := strconv.ParseInt(rev, 10, 64)
	k.workspace().put("child_done", float64(n), cfg(map[string]any{"enabled": false})).Want(http.StatusConflict)
}

func TestWakeupDefinitionListRedactsOnTheResolvedTarget(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD List Viewer", "member")
	private := dbfx.Agent(t, "wd list hidden agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": testUserID})
	squad := dbfx.Squad(t, "wd list hidden squad", private)
	cases := map[string]map[string]any{
		"pr_merged":        {"type": "agent", "id": private},
		"pr_checks_failed": {"type": "squad", "id": squad},
	}
	for rule, target := range cases {
		// The workspace names the private target; project and issue override only the prompt.
		k.workspace().put(rule, 0, cfg(map[string]any{"target": target, "instruction": "workspace secret"})).Want(http.StatusOK)
		k.project().put(rule, 0, cfg(map[string]any{"instruction": "project secret"})).Want(http.StatusOK)
		k.issue().put(rule, 0, cfg(map[string]any{"instruction": "issue secret"})).Want(http.StatusOK)
	}
	for name, s := range map[string]wdScope{"workspace": k.workspace(), "project": k.project(), "issue": k.issue()} {
		list := s.list(asUser(member)).Definitions
		if len(list) < 2 {
			t.Fatalf("%s: expected both rules listed, got %+v", name, list)
		}
		for _, view := range list {
			body := string(view.Config)
			if !view.Redacted || strings.Contains(body, "secret") || strings.Contains(body, private) || strings.Contains(body, squad) {
				t.Errorf("%s list leaks %s to an ordinary member: %+v", name, view.RuleKey, view)
			}
		}
		// The owner sees everything, and the patch stays sparse.
		for _, view := range s.list().Definitions {
			if view.Redacted || !strings.Contains(string(view.Config), "secret") {
				t.Errorf("%s list hides %s from the owner: %+v", name, view.RuleKey, view)
			}
		}
	}
	// A project override alone must not carry the inherited target into the sparse patch.
	for _, view := range k.project().list().Definitions {
		if strings.Contains(string(view.Config), `"target"`) {
			t.Errorf("project patch is no longer sparse: %s", view.Config)
		}
	}
}

// failDefinitionInserts makes every definition insert of the test workspace
// fail, after the settings aliases have been written in the same request.
func failDefinitionInserts(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `CREATE OR REPLACE FUNCTION wd_force_failure() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'forced definition write failure'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `CREATE TRIGGER wd_force_failure BEFORE INSERT OR UPDATE ON issue_wakeup_definition FOR EACH ROW WHEN (NEW.workspace_id = '`+testWorkspaceID+`') EXECUTE FUNCTION wd_force_failure()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS wd_force_failure ON issue_wakeup_definition`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS wd_force_failure()`)
	})
}

func TestWakeupDefinitionAliasAndRowWriteAreAtomic(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})
	// A parent whose child_done instance follows the workspace default.
	fx := newChildDoneFixture(t, "in_progress")
	updateChildStatus(t, fx.child.ID, "done") // the first sub-issue change creates the instance
	ruleEnabled := func() bool {
		var enabled bool
		dbfx.QueryRow(t, `SELECT enabled FROM issue_wakeup WHERE issue_id = $1 AND system_rule = 'child_done'`, fx.parent.ID).Scan(&enabled)
		return enabled
	}
	if !ruleEnabled() {
		t.Fatal("fixture instance should start enabled")
	}

	failDefinitionInserts(t)
	resp := k.workspace().put("child_done", 0, cfg(map[string]any{"enabled": false, "instruction": "never applied", "name": "label"}))
	if resp.Code == http.StatusOK {
		t.Fatalf("the forced row failure was not reported: %s", resp.Text())
	}
	var after string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&after)
	if after != settings {
		t.Fatalf("a failed row write left the alias changed:\n%s\n%s", settings, after)
	}
	if !ruleEnabled() {
		t.Fatal("a failed row write left the legacy child_done instance disabled")
	}
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d definitions persisted", n)
	}
}

// A settings writer that commits between the caller's observation and the
// write must make the write conflict, not be overwritten.
func TestWakeupDefinitionAliasWriteWaitsForSettingsWriters(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})

	ctx := context.Background()
	writer, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	// A settings writer's own UPDATE is what takes the row lock.
	if _, err := writer.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings,'{}'::jsonb) || '{"system_wakeup_child_done": false}'::jsonb WHERE id = $1`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- k.workspace().put("child_done", 0, cfg(map[string]any{"enabled": true, "instruction": "late writer"})).Code
	}()
	time.Sleep(400 * time.Millisecond)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusConflict {
		t.Fatalf("write racing a settings writer: got %d, want 409", code)
	}
	var raw string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&raw)
	if strings.Contains(raw, "late writer") || !strings.Contains(raw, `"system_wakeup_child_done": false`) {
		t.Fatalf("the racing write overwrote the settings writer: %s", raw)
	}
}

func TestWakeupDefinitionNullOnlyAliasClearSucceeds(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})

	// pr_merged: set the alias, then clear it with a null-only patch.
	k.workspace().put("pr_merged", 0, cfg(map[string]any{"enabled": false})).Want(http.StatusOK)
	var rev string
	var list wakeupDefinitionListResponse
	k.workspace().call(k.workspace().api().List, "GET", "", nil, nil).Want(http.StatusOK).JSON(&list)
	for _, d := range list.Definitions {
		if d.RuleKey == "pr_merged" {
			rev = strconv.FormatInt(int64(d.Revision), 10)
		}
	}
	var cleared wakeupDefinitionResponse
	k.workspace().put("pr_merged", rev, cfg(map[string]any{"enabled": nil})).Want(http.StatusOK).JSON(&cleared)
	if cleared.Revision != 0 || string(cleared.Config) != `{"v":1}` {
		t.Fatalf("cleared view = %+v %s", cleared, cleared.Config)
	}
	// child_done: an instruction-only clear with no enabled alias set.
	k.workspace().put("child_done", 0, cfg(map[string]any{"instruction": "to clear"})).Want(http.StatusOK)
	var list2 wakeupDefinitionListResponse
	k.workspace().call(k.workspace().api().List, "GET", "", nil, nil).Want(http.StatusOK).JSON(&list2)
	var childRev string
	for _, d := range list2.Definitions {
		if d.RuleKey == "child_done" {
			childRev = strconv.FormatInt(int64(d.Revision), 10)
		}
	}
	k.workspace().put("child_done", childRev, cfg(map[string]any{"instruction": nil})).Want(http.StatusOK)
	if n := wdCount(t); n != 0 {
		t.Fatalf("a clear stored %d rows", n)
	}
}

func TestWakeupDefinitionIssueDeleteRetiresTheLegacyOverride(t *testing.T) {
	for name, paused := range map[string]bool{"inherits": false, "keeps a safety pause": true} {
		t.Run(name, func(t *testing.T) {
			k := newWakeupDefinitionKit(t)
			fx := newChildDoneFixture(t, "in_progress")
			dbfx.Exec(t, `UPDATE issue SET project_id = $1 WHERE id = $2`, k.projectID, fx.parent.ID)
			issue := wdScope{t: t, kind: service.WakeupScopeIssue, params: []string{"id", fx.parent.ID}}
			// The project supplies the inherited instruction.
			k.project().put("child_done", 0, cfg(map[string]any{"instruction": "Project text"})).Want(http.StatusOK)
			// A pre-existing customized legacy override, as the old endpoint writes it.
			if w := putChildDoneRule(t, fx.parent.ID, map[string]any{"enabled": true, "instruction": "Legacy A"}); w.Code != http.StatusOK {
				t.Fatalf("legacy write: %d %s", w.Code, w.Body.String())
			}
			if paused {
				dbfx.Exec(t, `UPDATE issue_wakeup SET paused_reason = 'loop', enabled = false, fire_count = 3 WHERE issue_id = $1 AND system_rule = 'child_done'`, fx.parent.ID)
			}
			issue.put("child_done", 0, cfg(map[string]any{"instruction": "Stored B"})).Want(http.StatusOK)
			if got := issue.effective("child_done").Sources["instruction"]; got != "issue" {
				t.Fatalf("source before delete = %s", got)
			}
			var stored wakeupDefinitionListResponse
			issue.call(issue.api().List, "GET", "", nil, nil).JSON(&stored)
			issue.del("child_done", strconv.FormatInt(int64(stored.Definitions[0].Revision), 10)).Want(http.StatusNoContent)

			eff := issue.effective("child_done")
			if eff.Sources["instruction"] != "project" || !strings.Contains(string(eff.Config), "Project text") || strings.Contains(string(eff.Config), "Legacy A") {
				t.Fatalf("delete resurrected the legacy override: %s %v", eff.Config, eff.Sources)
			}
			var customized bool
			var instruction string
			var enabled bool
			var reason *string
			var fires int
			dbfx.QueryRow(t, `SELECT customized_at IS NOT NULL, instruction, enabled, paused_reason, fire_count FROM issue_wakeup WHERE issue_id = $1 AND system_rule = 'child_done'`, fx.parent.ID).
				Scan(&customized, &instruction, &enabled, &reason, &fires)
			if customized || instruction != "" {
				t.Fatalf("legacy override not retired: customized=%v instruction=%q", customized, instruction)
			}
			if paused && (reason == nil || *reason != "loop" || enabled || fires != 3) {
				t.Fatalf("safety pause or counters changed: enabled=%v reason=%v fires=%d", enabled, reason, fires)
			}
			if !paused && !enabled {
				t.Fatal("an unpaused row must follow the inherited enabled state")
			}
		})
	}
}

// ---- correction round 2 ----------------------------------------------------

// What a preview reports must be what saving the same proposal produces, and
// what the effective read then says.
func TestWakeupDefinitionPreviewSaveAndEffectiveAgree(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})

	revisionOf := func(rule string) string {
		for _, d := range k.workspace().list().Definitions {
			if d.RuleKey == rule {
				return strconv.FormatInt(int64(d.Revision), 10)
			}
		}
		return "0"
	}
	cases := []struct {
		name, rule string
		set, clear map[string]any
	}{
		{"pr_merged enabled", "pr_merged", map[string]any{"enabled": false}, map[string]any{"enabled": nil}},
		{"child_done enabled", "child_done", map[string]any{"enabled": false}, map[string]any{"enabled": nil}},
		{"child_done instruction", "child_done", map[string]any{"instruction": "  alias text  "}, map[string]any{"instruction": nil}},
		{"child_done replace keeps nothing", "child_done", map[string]any{"enabled": false, "instruction": "x"}, map[string]any{"name": "only a label"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k.workspace().put(c.rule, revisionOf(c.rule), cfg(c.set)).Want(http.StatusOK)
			var preview wakeupEffectiveResponse
			k.workspace().preview(c.rule, cfg(c.clear)).Want(http.StatusOK).JSON(&preview)
			before := wdCount(t)
			k.workspace().put(c.rule, revisionOf(c.rule), cfg(c.clear)).Want(http.StatusOK)
			effective := k.workspace().effective(c.rule)
			if preview.Enabled != effective.Enabled || string(preview.Config) != string(effective.Config) {
				t.Fatalf("preview and the saved state disagree:\n preview   enabled=%v %s\n effective enabled=%v %s", preview.Enabled, preview.Config, effective.Enabled, effective.Config)
			}
			if len(preview.Sources) != len(effective.Sources) {
				t.Fatalf("sources differ: preview %v, effective %v", preview.Sources, effective.Sources)
			}
			for field, scope := range preview.Sources {
				if effective.Sources[field] != scope {
					t.Fatalf("source of %s: preview %s, effective %s", field, scope, effective.Sources[field])
				}
			}
			_ = before
		})
	}
}

// A list that cannot resolve a definition is an error, not a permission-like
// redaction the viewer would read as "you may not see this".
func TestWakeupDefinitionListFailsLoudlyWhenResolutionFails(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	rule := "00000000-0000-4000-8000-0000000000aa"
	// Two roots of one custom rule cannot resolve: a second root is refused.
	for _, row := range []struct{ kind, id string }{{"workspace", testWorkspaceID}, {"project", k.projectID}} {
		dbfx.Exec(t, `INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config) VALUES($1,$2,$3,$4,true,'{"v":1,"name":"x"}'::jsonb)`,
			testWorkspaceID, row.kind, row.id, rule)
	}
	resp := k.project().call(k.project().api().List, "GET", "", nil, nil)
	if resp.Code == http.StatusOK {
		t.Fatalf("an unresolvable list answered 200: %s", resp.Text())
	}
	if strings.Contains(resp.Text(), `"redacted"`) {
		t.Fatalf("an error must not look like redaction: %s", resp.Text())
	}
}

func TestWakeupDefinitionTrimsBranchesAndNamesTheUnsupportedField(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	saved := decodeDefinition(t, k.project().put("pr_merged", 0, cfg(map[string]any{"filters": map[string]any{"base_branch": "  main ", "head_branch": "feature/x"}})).Want(http.StatusOK))
	if !strings.Contains(string(saved.Config), `"base_branch":"main"`) {
		t.Fatalf("branch not stored trimmed: %s", saved.Config)
	}
	k.project().put("pr_merged", saved.Revision, cfg(map[string]any{"filters": map[string]any{"base_branch": "   "}})).Want(http.StatusBadRequest)
	for i := 0; i < 20; i++ {
		resp := k.project().put("pr_merged", 0, cfg(map[string]any{"schedule": map[string]any{}, "active_run": "defer"})).Want(http.StatusBadRequest)
		if !strings.Contains(resp.Text(), "schedule") {
			t.Fatalf("the error must name the unsupported field every time: %s", resp.Text())
		}
	}
}

// ---- addendum: machine credentials and the resolved target -----------------

func asActorSource(source string) wdAs {
	return func(r *http.Request) *http.Request { r.Header.Set("X-Actor-Source", source); return r }
}

// A machine credential (a task token or a cloud-node PAT) carries its owner's
// user id, so a role check alone would let it write as that human. Writes and
// previews (which probe what the owner may invoke) refuse it at every scope.
func TestWakeupDefinitionRefusesMachineCredentials(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	own := handlerTestAgentID(t)
	for _, source := range []string{"task_token", "cloud_pat"} {
		for _, s := range []wdScope{k.workspace(), k.project(), k.issue()} {
			as := asActorSource(source)
			label := source + " at " + string(s.kind)
			config := cfg(map[string]any{"instruction": "from a machine", "target": map[string]any{"type": "agent", "id": own}})
			if resp := s.put("child_done", 0, config, as); resp.Code != http.StatusForbidden {
				t.Errorf("%s PUT: got %d: %s", label, resp.Code, resp.Text())
			}
			if resp := s.del("child_done", "1", as); resp.Code != http.StatusForbidden {
				t.Errorf("%s DELETE: got %d: %s", label, resp.Code, resp.Text())
			}
			if resp := s.preview("child_done", config, as); resp.Code != http.StatusForbidden {
				t.Errorf("%s preview: got %d: %s", label, resp.Code, resp.Text())
			}
			if s.kind != service.WakeupScopeIssue {
				create := cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "x"})
				if resp := s.create(create, as); resp.Code != http.StatusForbidden {
					t.Errorf("%s POST: got %d: %s", label, resp.Code, resp.Text())
				}
			}
		}
	}
	if n := wdCount(t); n != 0 {
		t.Fatalf("%d definitions persisted by machine credentials", n)
	}
	// The same human over an ordinary session still writes.
	k.project().put("child_done", 0, cfg(map[string]any{"name": "human"})).Want(http.StatusOK)
}

// A sparse override inherits its target. Writing an instruction that will run
// on an agent the writer may not invoke would let them steer it, so the
// resolved target is authorized when the result is enabled.
func TestWakeupDefinitionAuthorizesTheResolvedTarget(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	member := wdMember(t, "WD Inheritor", "member")
	private := dbfx.Agent(t, "wd inherited agent", handlerTestRuntimeID(t), testutil.Cols{"owner_id": testUserID})
	squad := dbfx.Squad(t, "wd inherited squad", private)
	for rule, target := range map[string]map[string]any{
		"pr_merged":        {"type": "agent", "id": private},
		"pr_checks_failed": {"type": "squad", "id": squad},
	} {
		k.workspace().put(rule, 0, cfg(map[string]any{"target": target})).Want(http.StatusOK)

		// Sparse overrides that leave the rule enabled are refused for a member who cannot invoke the target.
		for name, sparse := range map[string]map[string]any{
			"instruction": {"instruction": "steer it"},
			"mode":        {"mode": "continuous"},
			"enabled":     {"enabled": true},
		} {
			if resp := k.issue().put(rule, 0, cfg(sparse), asUser(member)); resp.Code != http.StatusForbidden {
				t.Errorf("%s: %s override by a member who cannot invoke the target: got %d: %s", rule, name, resp.Code, resp.Text())
			}
			if resp := k.issue().preview(rule, cfg(sparse), asUser(member)); resp.Code != http.StatusForbidden {
				t.Errorf("%s: %s preview: got %d", rule, name, resp.Code)
			}
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id = $1 AND scope_kind = 'issue' AND rule_key = $2`, testWorkspaceID, rule); n != 0 {
			t.Fatalf("%s: %d issue overrides persisted from denied writes", rule, n)
		}
		// Turning the inherited rule off runs nothing, so it stays allowed, and so does a
		// person who can invoke the target.
		resp := k.issue().put(rule, 0, cfg(map[string]any{"enabled": false}), asUser(member)).Want(http.StatusOK)
		k.issue().del(rule, strconv.FormatInt(int64(decodeDefinition(t, resp).Revision), 10), asUser(member)).Want(http.StatusNoContent)
		k.issue().put(rule, 0, cfg(map[string]any{"instruction": "owner text"})).Want(http.StatusOK)
	}
}

// ---- round 3: workspace settings that are not an object --------------------

// The owner/admin workspace update stores `settings` unchecked, so an array, a
// scalar or JSON null is reachable. An alias write merges an object into the
// settings, which cannot be simulated (or applied) on those shapes: they are
// refused before anything persists, and preview, save and effective agree.
func TestWakeupDefinitionRefusesAliasWritesOnNonObjectSettings(t *testing.T) {
	var original string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, original, testWorkspaceID)
	})

	storeViaWorkspaceUpdate := func(t *testing.T) {
		w := httptest.NewRecorder()
		req := withURLParam(newRequest("PUT", "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": []any{}}), "id", testWorkspaceID)
		testHandler.UpdateWorkspace(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("workspace update: %d %s", w.Code, w.Body.String())
		}
	}
	storeRaw := func(literal string) func(*testing.T) {
		return func(t *testing.T) {
			dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, literal, testWorkspaceID)
		}
	}
	for name, store := range map[string]func(*testing.T){
		"array via the workspace update": storeViaWorkspaceUpdate,
		"array":                          storeRaw(`[]`),
		"scalar number":                  storeRaw(`5`),
		"scalar string":                  storeRaw(`"text"`),
		"json null":                      storeRaw(`null`),
	} {
		t.Run(name, func(t *testing.T) {
			k := newWakeupDefinitionKit(t)
			store(t)
			var stored string
			dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&stored)
			for _, rule := range []string{"child_done", "pr_merged"} {
				alias := cfg(map[string]any{"enabled": false})
				k.workspace().preview(rule, alias).Want(http.StatusBadRequest)
				k.workspace().put(rule, "0", alias).Want(http.StatusBadRequest)
			}
			k.workspace().put("child_done", "0", cfg(map[string]any{"instruction": "alias text"})).Want(http.StatusBadRequest)
			var after string
			dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&after)
			if after != stored || wdCount(t) != 0 {
				t.Fatalf("a refused write changed state: settings %s -> %s, %d definitions", stored, after, wdCount(t))
			}
			// The effective read still resolves the built-in default, and a write that
			// touches no alias field has nothing to merge, so it is not refused.
			if eff := k.workspace().effective("child_done"); !eff.Enabled {
				t.Fatalf("effective read: %+v", eff)
			}
			k.workspace().put("child_done", "0", cfg(map[string]any{"name": "label only"})).Want(http.StatusOK)
		})
	}
}

// ---- round 4: the per-scope ceiling counts stored records only -------------

func workspaceRevision(k *wdKit, rule string) string {
	for _, d := range k.workspace().list().Definitions {
		if d.RuleKey == rule {
			return strconv.FormatInt(int64(d.Revision), 10)
		}
	}
	return "0"
}

func fillWorkspaceDefinitions(t *testing.T, k *wdKit, n int) {
	t.Helper()
	for i := range n {
		k.workspace().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "filler " + strconv.Itoa(i)})).Want(http.StatusCreated)
	}
}

// An alias-only update of a workspace built-in stores no row, so the 32-record
// ceiling must not stop it: at the ceiling a workspace can still toggle,
// disable and clear its built-ins. Only a write that inserts a record counts.
func TestWakeupDefinitionAliasOnlyWritesWorkAtTheCeiling(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	var settings string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1::jsonb WHERE id = $2`, settings, testWorkspaceID)
	})

	// The alias exists first and stores no row; then the scope fills to 32.
	k.workspace().put("pr_merged", "0", cfg(map[string]any{"enabled": false})).Want(http.StatusOK)
	fillWorkspaceDefinitions(t, k, 32)
	if n := wdCount(t); n != 32 {
		t.Fatalf("setup: %d stored records, want 32", n)
	}

	// Toggle the built-in back on, echoing the decimal-string revision LIST gave.
	k.workspace().put("pr_merged", workspaceRevision(k, "pr_merged"), cfg(map[string]any{"enabled": true})).Want(http.StatusOK)
	if n := wdCount(t); n != 32 {
		t.Fatalf("an alias-only write changed the stored count to %d", n)
	}

	// Alias-only sets and clears of all three built-ins.
	for _, rule := range []string{"pr_merged", "pr_checks_failed", "child_done"} {
		k.workspace().put(rule, workspaceRevision(k, rule), cfg(map[string]any{"enabled": false})).Want(http.StatusOK)
		k.workspace().put(rule, workspaceRevision(k, rule), cfg(map[string]any{"enabled": nil})).Want(http.StatusOK)
	}
	k.workspace().put("child_done", workspaceRevision(k, "child_done"), cfg(map[string]any{"instruction": "alias text"})).Want(http.StatusOK)
	k.workspace().put("child_done", workspaceRevision(k, "child_done"), cfg(map[string]any{"instruction": nil})).Want(http.StatusOK)
	if n := wdCount(t); n != 32 {
		t.Fatalf("alias-only writes left %d stored records", n)
	}

	// Negative control: a write that would store a 33rd record is still refused,
	// and its alias effects roll back with it.
	var before string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&before)
	k.workspace().create(cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "33rd"})).Want(http.StatusBadRequest)
	k.workspace().put("pr_checks_failed", workspaceRevision(k, "pr_checks_failed"), cfg(map[string]any{"enabled": false, "name": "needs a row"})).Want(http.StatusBadRequest)
	var after string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&after)
	if after != before || wdCount(t) != 32 {
		t.Fatalf("a refused 33rd record changed state: settings %s -> %s, %d records", before, after, wdCount(t))
	}
	// An existing record still updates at the ceiling.
	var first wakeupDefinitionResponse
	for _, d := range k.workspace().list().Definitions {
		if d.Root {
			first = d
			break
		}
	}
	k.workspace().put(first.RuleKey, strconv.FormatInt(int64(first.Revision), 10), cfg(map[string]any{"trigger": map[string]any{"kind": "pr_merged"}, "instruction": "edited"})).Want(http.StatusOK)
}

// ---- custom event and condition triggers (L10) -----------------------------

func TestWakeupDefinitionCustomTriggersAreAcceptedBehindTheGate(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	caps := k.workspace().list().Capabilities
	for _, kind := range []string{"event", "condition"} {
		if !slices.Contains(caps.TriggerKinds, kind) {
			t.Fatalf("trigger kinds %v lack %q", caps.TriggerKinds, kind)
		}
	}
	condition := map[string]any{"kind": "condition", "condition": map[string]any{"type": "issue_field", "field": "status", "value": "todo"}}
	body := cfg(map[string]any{"enabled": true, "trigger": condition, "instruction": "it is ready"})

	// The preview says how many issues already satisfy the predicate, and persists nothing.
	var preview wakeupEffectiveResponse
	k.project().preview("", body).Want(http.StatusOK).JSON(&preview)
	if preview.AlreadySatisfied == nil || preview.AlreadySatisfied.Satisfied != 1 || preview.AlreadySatisfied.Examined != 1 || preview.AlreadySatisfied.Truncated {
		t.Fatalf("already satisfied = %+v, want the project's one todo issue", preview.AlreadySatisfied)
	}
	if n := wdCount(t); n != 0 {
		t.Fatalf("a preview persisted %d definitions", n)
	}
	var event wakeupEffectiveResponse
	k.project().preview("", cfg(map[string]any{"enabled": true, "trigger": map[string]any{"kind": "event", "events": []string{"comment.created"}}, "instruction": "x"})).Want(http.StatusOK).JSON(&event)
	if event.AlreadySatisfied != nil {
		t.Fatalf("an event rule has no predicate to count, got %+v", event.AlreadySatisfied)
	}

	k.project().create(body).Want(http.StatusCreated)
	k.project().create(cfg(map[string]any{"trigger": map[string]any{"kind": "event", "events": []string{"not.an.event"}}, "instruction": "x"})).Want(http.StatusBadRequest)
	k.project().create(cfg(map[string]any{"trigger": map[string]any{"kind": "condition", "condition": map[string]any{"type": "nonsense"}}, "instruction": "x"})).Want(http.StatusBadRequest)
	if n := wdCount(t); n != 1 {
		t.Fatalf("%d definitions stored, want only the valid one", n)
	}

	// Behind the closed gate none of this is writable.
	withFeatureFlag(t, testHandler, featureflags.WakeupDefinitionWrites, false)
	k.project().create(body).Want(http.StatusForbidden)
}
