package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

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
	k.issueID = dbfx.Issue(t, "wd issue", testutil.Cols{"project_id": k.projectID})
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

func wdBody(revision int64, config map[string]any) map[string]any {
	return map[string]any{"revision": revision, "config": config}
}

func (s wdScope) put(rule string, revision int64, config map[string]any, as ...wdAs) *testutil.Response {
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
	k.issueID = dbfx.Issue(t, "wd issue", testutil.Cols{"project_id": k.projectID})
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
		"aggregate limit":          cfg(map[string]any{"aggregate_limit": 6}),
		"active run":               cfg(map[string]any{"active_run": "defer"}),
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
		"filters": map[string]any{"base_branch": "main", "ci": "failure", "priorities": []string{"urgent", "high"}},
	})).Want(http.StatusOK)
	// An unknown rule key is not a rule.
	k.project().put("not-a-rule", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusBadRequest)
}

func TestWakeupDefinitionCustomRuleRootAndOverrides(t *testing.T) {
	k := newWakeupDefinitionKit(t)
	root := decodeDefinition(t, k.workspace().create(cfg(map[string]any{
		"name": "PR watcher", "enabled": true, "trigger": map[string]any{"kind": "pr_merged"}, "instruction": "Summarise the merge.",
	})).Want(http.StatusCreated))
	if !root.Root || root.Revision != 1 || len(root.RuleKey) != 36 {
		t.Fatalf("root: %+v", root)
	}
	// Creation inserts no default: no aggregate cap, fire cap, mode or defer.
	keys := configKeys(t, root.Config)
	for _, banned := range []string{"aggregate_limit", "max_fires", "mode", "active_run", "rate_limit", "expiry"} {
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
