package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/githubrepo"
)

// CHE-9525: github.repo.create is the card an agent files instead of holding
// an org-admin credential. The repo exists, is listed in the guard allowlist
// and is in the workspace registry only after the owner approves it. The
// "sandbox org" is a stand-in GitHub API on httptest: it keeps the repos a
// request created, so the tests read the repo back through the same client the
// server uses.

const repoAdminSecret = "ghp_sentinel_org_admin_token"

type fakeGitHubOrg struct {
	mu      sync.Mutex
	repos   map[string]map[string]any // full name -> create body
	calls   []string
	auth    []string
	failure int // answer every POST with this status when set
	// getFailure answers every GET with this status when set.
	getFailure int
	// lostAnswer creates the repo and then answers every POST with this status:
	// GitHub did the work and the answer was lost.
	lostAnswer int
}

func (fake *fakeGitHubOrg) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, request.Method+" "+request.URL.Path)
	fake.auth = append(fake.auth, request.Header.Get("Authorization"))
	if request.Method == http.MethodGet {
		if fake.getFailure != 0 {
			writer.WriteHeader(fake.getFailure)
			return
		}
		name := strings.TrimPrefix(request.URL.Path, "/repos/")
		body, ok := fake.repos[name]
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		visibility := "private"
		if body["private"] == false || body["visibility"] == "public" {
			visibility = "public"
		}
		view := map[string]any{"full_name": name, "html_url": "https://github.com/" + name, "visibility": visibility, "description": body["description"]}
		_ = json.NewEncoder(writer).Encode(view)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(request.Body).Decode(&body)
	if fake.failure != 0 {
		writer.WriteHeader(fake.failure)
		return
	}
	org, template := "", ""
	if strings.HasSuffix(request.URL.Path, "/generate") {
		template = strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/repos/"), "/generate")
		org, _ = body["owner"].(string)
	} else {
		org = strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/orgs/"), "/repos")
	}
	full := org + "/" + body["name"].(string)
	if _, taken := fake.repos[full]; taken {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = writer.Write([]byte(`{"message":"Repository creation failed.","errors":[{"message":"name already exists on this account"}]}`))
		return
	}
	body["template"] = template
	fake.repos[full] = body
	if fake.lostAnswer != 0 {
		writer.WriteHeader(fake.lostAnswer)
		return
	}
	writer.WriteHeader(http.StatusCreated)
	_, _ = writer.Write([]byte(`{}`))
}

func (fake *fakeGitHubOrg) has(full string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	_, ok := fake.repos[full]
	return ok
}

func (fake *fakeGitHubOrg) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// repoCreateFixture wires the handler to the fake org and seeds the workspace.
type repoCreateFixture struct {
	pendingAction
	org     *fakeGitHubOrg
	handler *Handler
	name    string
}

// seedWorkspace sets the workspace settings and repos for the test and
// restores both afterwards.
func seedWorkspace(test *testing.T, settings, repos string) {
	test.Helper()
	var originalSettings, originalRepos string
	dbfx.QueryRow(test, `SELECT settings::text, repos::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&originalSettings, &originalRepos)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE workspace SET settings = $2::jsonb, repos = $3::jsonb WHERE id = $1`, testWorkspaceID, originalSettings, originalRepos)
	})
	dbfx.Exec(test, `UPDATE workspace SET settings = $2::jsonb, repos = $3::jsonb WHERE id = $1`, testWorkspaceID, settings, repos)
}

const listedTevoSettings = `{"guard_policy":{"version":1,"repository_allowlist":{"tevo-studio":["existing"]}}}`

func repoCreateParams(visibility, template string) map[string]any {
	params := map[string]any{"workspace_id": testWorkspaceID, "org": "tevo-studio", "name": "we006", "visibility": visibility, "description": "WE-006 app"}
	if template != "" {
		params["template"] = template
	}
	return params
}

func newRepoCreateFixture(test *testing.T, class string, params map[string]any) repoCreateFixture {
	test.Helper()
	org := &fakeGitHubOrg{repos: map[string]map[string]any{}}
	server := httptest.NewServer(org)
	test.Cleanup(server.Close)
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{"type": decisionaction.GitHubRepoCreate, "version": 1, "params": params})
	body["escalation_class"] = class
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	pending := pendingAction{fixture, created.ID, created.Action.ID}
	makeApproverOwner(test, pending, "owner")
	copied := *testHandler
	copied.GitHubRepoCreator = githubrepo.NewClient(server.URL, map[string]string{"tevo-studio": repoAdminSecret})
	return repoCreateFixture{pending, org, &copied, "we006"}
}

func (fixture repoCreateFixture) approve(test *testing.T) decisionActionOutcome {
	test.Helper()
	copied := *fixture.handler
	copied.ApprovalVerifier = acceptingVerifier
	var out decisionActionOutcome
	decodeJSON(test, fixture.pendingAction.approve(test, &copied, fixture.claim(test, map[string]string{}), http.StatusOK), &out)
	return out
}

func workspaceRows(test *testing.T) (settings, repos string) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT settings::text, repos::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&settings, &repos)
	return settings, repos
}

func TestGitHubRepoCreateIsRegisteredAsAnOwnerPasskeyActionWithAnExecutor(test *testing.T) {
	definition, ok := decisionaction.Lookup(decisionaction.GitHubRepoCreate, 1)
	if !ok {
		test.Fatal("github.repo.create is not on the allow table")
	}
	if definition.Passkey != decisionaction.PasskeyRequired || definition.MinClass != "spend" || !definition.OwnerOnly {
		test.Fatalf("definition = %+v, want passkey required, class spend, owner only", definition)
	}
	if _, ok := decisionActionExecutors[decisionaction.GitHubRepoCreate]; !ok {
		test.Fatal("github.repo.create has no executor")
	}
}

func TestGitHubRepoCreateApprovalCreatesTheRepoAndListsItEverywhere(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[{"url":"https://github.com/tevo-studio/existing.git","description":"kept"}]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", "tevo-studio/code-android-template"))

	// A proposal alone creates nothing.
	if fixture.org.callCount() != 0 || fixture.state(test) != "pending" {
		test.Fatalf("a pending card reached GitHub (%d calls) or left state %s", fixture.org.callCount(), fixture.state(test))
	}
	out := fixture.approve(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v, want executed", out)
	}

	// Read back: the repo exists, in the org, from the template, private.
	if !fixture.org.has("tevo-studio/we006") {
		test.Fatal("the repo does not exist on the org after approval")
	}
	if got := fixture.org.repos["tevo-studio/we006"]["template"]; got != "tevo-studio/code-android-template" {
		test.Fatalf("template = %v", got)
	}
	// The guard allowlist lists it, and the old entry survived.
	settings, repos := workspaceRows(test)
	var stored struct {
		GuardPolicy struct {
			Allowlist map[string][]string `json:"repository_allowlist"`
		} `json:"guard_policy"`
	}
	if err := json.Unmarshal([]byte(settings), &stored); err != nil {
		test.Fatal(err)
	}
	if got := strings.Join(stored.GuardPolicy.Allowlist["tevo-studio"], ","); got != "existing,we006" {
		test.Fatalf("allowlist = %q", got)
	}
	// The registry lists it, and the old entry survived.
	var registry []workspaceRepoRef
	if err := json.Unmarshal([]byte(repos), &registry); err != nil || len(registry) != 2 || registry[0].Description != "kept" || registry[1].URL != "https://github.com/tevo-studio/we006.git" {
		test.Fatalf("registry = %s (%v)", repos, err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Result, &result); err != nil || result["full_name"] != "tevo-studio/we006" || result["visibility"] != "private" || result["allowlist"] != "listed" || result["registry"] != "added" {
		test.Fatalf("result = %s (%v)", out.Result, err)
	}
}

func TestGitHubRepoCreateNeverLeaksTheAdminCredential(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	out := fixture.approve(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v, want executed", out)
	}
	// The server used the credential; nothing the agent or the card can read holds it.
	if fixture.org.auth[0] != "Bearer "+repoAdminSecret {
		test.Fatalf("GitHub was called with %q, want the org token", fixture.org.auth[0])
	}
	settings, repos := workspaceRows(test)
	var action, preview, result string
	dbfx.QueryRow(test, `SELECT params::text, preconditions::text, coalesce(result::text, '') FROM decision_action WHERE id = $1`, fixture.actionID).Scan(&action, &preview, &result)
	card := fixture.cardAction(test)
	rendered, _ := json.Marshal(card)
	for name, text := range map[string]string{"settings": settings, "repos": repos, "action params": action, "preconditions": preview, "result": result, "card": string(rendered), "outcome": string(out.Result)} {
		if strings.Contains(text, repoAdminSecret) {
			test.Fatalf("the admin credential reached the %s", name)
		}
	}
	var agentEnv string
	dbfx.QueryRow(test, `SELECT coalesce(custom_env::text, '') FROM agent WHERE id = $1`, fixture.agentID).Scan(&agentEnv)
	if strings.Contains(agentEnv, repoAdminSecret) {
		test.Fatal("the admin credential reached the agent")
	}
}

func TestGitHubRepoCreateRejectCreatesNothing(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	beforeSettings, beforeRepos := workspaceRows(test)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	fixture.reject(test, withVerifier(acceptingVerifier), http.StatusOK)
	fixture.assertNothingCreated(test, beforeSettings, beforeRepos)
}

func TestGitHubRepoCreateExpiredCardCreatesNothing(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	beforeSettings, beforeRepos := workspaceRows(test)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	dbfx.Exec(test, `UPDATE decision_action SET expires_at = clock_timestamp() - interval '1 minute' WHERE id = $1`, fixture.actionID)
	copied := *fixture.handler
	copied.ApprovalVerifier = acceptingVerifier
	fixture.pendingAction.approve(test, &copied, fixture.claim(test, map[string]string{}), http.StatusConflict)
	fixture.assertNothingCreated(test, beforeSettings, beforeRepos)
}

func (fixture repoCreateFixture) assertNothingCreated(test *testing.T, settings, repos string) {
	test.Helper()
	if fixture.org.callCount() != 0 || fixture.org.has("tevo-studio/we006") {
		test.Fatalf("GitHub saw %d calls; a refused card must create nothing", fixture.org.callCount())
	}
	if gotSettings, gotRepos := workspaceRows(test); gotSettings != settings || gotRepos != repos {
		test.Fatalf("workspace rows changed: settings %s, repos %s", gotSettings, gotRepos)
	}
}

func TestGitHubRepoCreateGitHubFailureRollsTheWorkspaceRowsBack(test *testing.T) {
	for name, tc := range map[string]struct {
		status int
		code   string
	}{
		"unauthorized":  {http.StatusUnauthorized, "github_create_failed"},
		"missing token": {0, "credential_not_configured"},
	} {
		test.Run(name, func(test *testing.T) {
			seedWorkspace(test, listedTevoSettings, `[]`)
			beforeSettings, beforeRepos := workspaceRows(test)
			fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
			fixture.org.failure = tc.status
			if tc.status == 0 {
				fixture.handler.GitHubRepoCreator = githubrepo.NewClient("http://127.0.0.1:1", map[string]string{})
			}
			out := fixture.approve(test)
			if out.State != "failed" || out.ErrorCode != tc.code {
				test.Fatalf("outcome = %+v, want failed/%s", out, tc.code)
			}
			if gotSettings, gotRepos := workspaceRows(test); gotSettings != beforeSettings || gotRepos != beforeRepos {
				test.Fatalf("a failed create left workspace rows changed: settings %s, repos %s", gotSettings, gotRepos)
			}
		})
	}
}

func TestGitHubRepoCreateEndsAsAFailureWhenTheNameIsTaken(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	beforeSettings, beforeRepos := workspaceRows(test)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	fixture.org.repos["tevo-studio/we006"] = map[string]any{}
	out := fixture.approve(test)
	if out.State != "failed" || out.ErrorCode != "repo_exists" {
		test.Fatalf("outcome = %+v, want failed/repo_exists", out)
	}
	if gotSettings, gotRepos := workspaceRows(test); gotSettings != beforeSettings || gotRepos != beforeRepos {
		test.Fatal("a name clash left workspace rows changed")
	}
}

// A create GitHub accepted must not be undone because the read-back failed.
func TestGitHubRepoCreateKeepsTheRepoWhenTheReadBackFails(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	fixture.org.getFailure = http.StatusBadGateway
	out := fixture.approve(test)
	if out.State != "executed" || !fixture.org.has("tevo-studio/we006") {
		test.Fatalf("outcome = %+v, repo exists = %v", out, fixture.org.has("tevo-studio/we006"))
	}
	settings, repos := workspaceRows(test)
	if !strings.Contains(settings, "we006") || !strings.Contains(repos, "tevo-studio/we006") {
		test.Fatalf("the repo is not in both lists: settings %s, repos %s", settings, repos)
	}
}

// GitHub created the repo but the answer was lost and the read-back fails too.
// The card stays pending with nothing listed, and deciding the same card again
// adopts the repo by its marker and lists it everywhere.
func TestGitHubRepoCreateRecoversAfterALostAnswerOnTheSameCard(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	beforeSettings, beforeRepos := workspaceRows(test)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", "tevo-studio/code-android-template"))
	fixture.org.lostAnswer, fixture.org.getFailure = http.StatusBadGateway, http.StatusBadGateway
	copied := *fixture.handler
	copied.ApprovalVerifier = acceptingVerifier
	fixture.pendingAction.approve(test, &copied, fixture.claim(test, map[string]string{}), http.StatusServiceUnavailable)
	if !fixture.org.has("tevo-studio/we006") {
		test.Fatal("the repo was not created")
	}
	var state string
	dbfx.QueryRow(test, `SELECT state FROM decision_action WHERE id = $1`, fixture.actionID).Scan(&state)
	if state != "pending" {
		test.Fatalf("the card state = %q after a lost answer, want pending", state)
	}
	if gotSettings, gotRepos := workspaceRows(test); gotSettings != beforeSettings || gotRepos != beforeRepos {
		test.Fatalf("a lost answer changed workspace rows: settings %s, repos %s", gotSettings, gotRepos)
	}
	fixture.org.lostAnswer, fixture.org.getFailure = 0, 0
	out := fixture.approve(test)
	if out.State != "executed" {
		test.Fatalf("retry outcome = %+v, want executed", out)
	}
	settings, repos := workspaceRows(test)
	if !strings.Contains(settings, "we006") || !strings.Contains(repos, "tevo-studio/we006") {
		test.Fatalf("the adopted repo is not in both lists: settings %s, repos %s", settings, repos)
	}
}

// A repo that already carries the name is not this action's unless its
// description carries this action's marker.
func TestGitHubRepoCreateDoesNotAdoptAnUnrelatedRepo(test *testing.T) {
	for name, description := range map[string]string{
		"no marker":             "WE-006 app",
		"another card's":        "WE-006 app [multica-action:00000000-0000-0000-0000-000000000000]",
		"marker not at the end": "[multica-action:ACTION] WE-006 app",
	} {
		test.Run(name, func(test *testing.T) {
			seedWorkspace(test, listedTevoSettings, `[]`)
			beforeSettings, beforeRepos := workspaceRows(test)
			fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
			description = strings.ReplaceAll(description, "ACTION", fixture.actionID)
			fixture.org.repos["tevo-studio/we006"] = map[string]any{"private": true, "description": description}
			out := fixture.approve(test)
			if out.State != "failed" || out.ErrorCode != "repo_exists" {
				test.Fatalf("outcome = %+v, want failed/repo_exists", out)
			}
			if gotSettings, gotRepos := workspaceRows(test); gotSettings != beforeSettings || gotRepos != beforeRepos {
				test.Fatal("an unrelated repo was adopted into the workspace rows")
			}
		})
	}
}

func TestGitHubRepoCreateEndsAsAFailureWithoutAConfiguredCreator(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	fixture.handler.GitHubRepoCreator = nil
	out := fixture.approve(test)
	if out.State != "failed" || out.ErrorCode != "github_not_configured" {
		test.Fatalf("outcome = %+v, want failed/github_not_configured", out)
	}
}

func TestGitHubRepoCreateLeavesAnUnrestrictedOrgAlone(test *testing.T) {
	// No allowlist entry for the org: every repo is already reachable, and an
	// entry would lock the rest of the org out.
	seedWorkspace(test, `{"guard_policy":{"version":1,"repository_allowlist":{"other-org":["x"]}}}`, `[]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	if fields := previewFields(test, fixture.cardAction(test)); fields["Guard allowlist"][1] != "not restricted for this org" {
		test.Fatalf("preview allowlist = %v", fields["Guard allowlist"])
	}
	before, _ := workspaceRows(test)
	out := fixture.approve(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v, want executed", out)
	}
	after, repos := workspaceRows(test)
	if after != before {
		test.Fatalf("settings changed for an unrestricted org: %s", after)
	}
	if !strings.Contains(repos, "https://github.com/tevo-studio/we006.git") {
		test.Fatalf("registry = %s, want the repo listed", repos)
	}
}

func TestGitHubRepoCreateApprovalIsIdempotentForAListedRepo(test *testing.T) {
	seedWorkspace(test, `{"guard_policy":{"version":1,"repository_allowlist":{"tevo-studio":["we006"]}}}`, `[{"url":"https://github.com/tevo-studio/we006.git"}]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", ""))
	before, beforeRepos := workspaceRows(test)
	if out := fixture.approve(test); out.State != "executed" {
		test.Fatalf("outcome = %+v, want executed", out)
	}
	after, afterRepos := workspaceRows(test)
	if strings.Count(afterRepos, "we006.git") != 1 || after != before || beforeRepos == "" {
		test.Fatalf("an already listed repo was listed twice: settings %s, repos %s", after, afterRepos)
	}
}

func TestGitHubRepoCreateCardShowsWhatApprovalCreates(test *testing.T) {
	seedWorkspace(test, listedTevoSettings, `[]`)
	fixture := newRepoCreateFixture(test, "spend", repoCreateParams("private", "tevo-studio/code-android-template"))
	card := fixture.cardAction(test)
	fields := previewFields(test, card)
	assertPreviewDigest(test, card.Preview)
	for label, want := range map[string][2]string{
		"Repository":              {"does not exist", "tevo-studio/we006"},
		"Visibility":              {"", "private"},
		"Template":                {"", "tevo-studio/code-android-template"},
		"Guard allowlist":         {"not listed", "listed"},
		"Workspace repo registry": {"not listed", "listed"},
	} {
		if fields[label] != want {
			test.Fatalf("%s = %v, want %v", label, fields[label], want)
		}
	}
}

func TestGitHubRepoCreateProposalRefusals(test *testing.T) {
	fixture := newActionFixture(test)
	for name, tc := range map[string]struct {
		class  string
		edit   func(map[string]any)
		status int
	}{
		"org not approved":                {"spend", func(params map[string]any) { params["org"] = "evil-org" }, http.StatusBadRequest},
		"template from an unapproved org": {"spend", func(params map[string]any) { params["template"] = "evil-org/template" }, http.StatusBadRequest},
		"public without class publish":    {"spend", func(params map[string]any) { params["visibility"] = "public" }, http.StatusBadRequest},
		"public with class reversible":    {"reversible", func(params map[string]any) { params["visibility"] = "public" }, http.StatusBadRequest},
		"public with class publish":       {"publish", func(params map[string]any) { params["visibility"] = "public" }, http.StatusCreated},
		"private with class spend":        {"spend", func(params map[string]any) {}, http.StatusCreated},
		"unknown visibility":              {"spend", func(params map[string]any) { params["visibility"] = "secret" }, http.StatusBadRequest},
		"foreign workspace":               {"spend", func(params map[string]any) { params["workspace_id"] = "00000000-0000-4000-8000-000000000000" }, http.StatusBadRequest},
		"bad name":                        {"spend", func(params map[string]any) { params["name"] = "../x" }, http.StatusBadRequest},
		"extra field":                     {"spend", func(params map[string]any) { params["token"] = "x" }, http.StatusBadRequest},
	} {
		test.Run(name, func(test *testing.T) {
			params := repoCreateParams("private", "")
			tc.edit(params)
			body := fixture.withAction(map[string]any{"type": decisionaction.GitHubRepoCreate, "version": 1, "params": params})
			body["escalation_class"] = tc.class
			ask := fixture.details
			ask.Decision = "Create a repository? (" + name + ")" // the same ask text is one request
			body["ask"] = ask
			var created actionResponse
			fixture.respond(test, body, tc.status, &created)
			if tc.status == http.StatusCreated {
				dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
			}
		})
	}
}
