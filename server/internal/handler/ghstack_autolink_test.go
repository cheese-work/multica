package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-4165 (CHE-4143 step 5): the read and write API behind
// `multica github stack-autolink status|set`.

const stackAutolinkInstallation int64 = 4165001

type stackAutolinkStatus struct {
	Repo             string `json:"repo"`
	FinalMode        string `json:"final_mode"`
	EnvKill          bool   `json:"env_kill"`
	HiddenWorkspaces int    `json:"hidden_workspaces"`
	Workspaces       []struct {
		WorkspaceID   string `json:"workspace_id"`
		EffectiveMode string `json:"effective_mode"`
	} `json:"workspaces"`
}

type stackAutolinkSet struct {
	WorkspaceID   string `json:"workspace_id"`
	Repo          string `json:"repo"`
	Mode          string `json:"mode"`
	EffectiveMode string `json:"effective_mode"`
}

// stackWorkspace makes a workspace bound to the test installation for owner
// "acme". role "" means the test user is not a member.
func stackWorkspace(t *testing.T, slug, role string, settings map[string]any) string {
	t.Helper()
	ws := dbfx.Workspace(t, slug, slug)
	if role != "" {
		dbfx.Member(t, ws, testUserID, role)
	}
	raw, _ := json.Marshal(settings)
	dbfx.Exec(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, ws, string(raw))
	dbfx.Exec(t, `INSERT INTO github_installation (workspace_id, installation_id, account_login, account_type) VALUES ($1, $2, 'acme', 'Organization')`, ws, stackAutolinkInstallation)
	dbfx.Cleanup(t, `DELETE FROM github_installation WHERE workspace_id = $1`, ws)
	dbfx.Cleanup(t, `DELETE FROM workspace_audit_event WHERE workspace_id = $1`, ws)
	return ws
}

func stackRequest(method, path string, body any, wsID string) *http.Request {
	req := newRequest(method, path, body)
	req.Header.Set("X-Workspace-ID", wsID)
	return req
}

func stackStatus(t *testing.T, wsID, query string) stackAutolinkStatus {
	t.Helper()
	var out stackAutolinkStatus
	testutil.Call(t, testHandler.GetGitHubStackAutolink, stackRequest("GET", "/api/github/stack-autolink"+query, nil, wsID)).Want(http.StatusOK).JSON(&out)
	return out
}

func storedStackAutolink(t *testing.T, wsID string) map[string]any {
	t.Helper()
	var raw []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, wsID).Scan(&raw)
	var settings map[string]any
	_ = json.Unmarshal(raw, &settings)
	return settings
}

func TestGitHubStackAutolinkStatusPerMode(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{"no key defaults to report", map[string]any{}, "report"},
		{"off", map[string]any{"github_stack_autolink": map[string]any{"mode": "off"}}, "off"},
		{"report", map[string]any{"github_stack_autolink": map[string]any{"mode": "report"}}, "report"},
		{"link", map[string]any{"github_stack_autolink": map[string]any{"mode": "link"}}, "link"},
		{"repo entry raises report to link", map[string]any{"github_stack_autolink": map[string]any{"mode": "report", "repos": map[string]any{"acme/app": "link"}}}, "link"},
		{"workspace off beats a repo entry", map[string]any{"github_stack_autolink": map[string]any{"mode": "off", "repos": map[string]any{"acme/app": "link"}}}, "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := stackWorkspace(t, "ghs-status-"+tc.want, "member", tc.settings)
			got := stackStatus(t, ws, "?repo=Acme/App")
			if got.FinalMode != tc.want || got.Repo != "acme/app" {
				t.Fatalf("final_mode=%q repo=%q, want %q acme/app", got.FinalMode, got.Repo, tc.want)
			}
			if len(got.Workspaces) != 1 || got.Workspaces[0].WorkspaceID != ws || got.Workspaces[0].EffectiveMode != tc.want {
				t.Fatalf("workspaces = %+v, want one entry %s=%s", got.Workspaces, ws, tc.want)
			}
		})
	}
}

// The final mode is the lowest across every bound workspace; the caller only
// sees the workspaces they belong to, and a hidden one is counted, not named.
func TestGitHubStackAutolinkStatusLowestWinsAndHidesForeignWorkspaces(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	link := map[string]any{"github_stack_autolink": map[string]any{"mode": "link"}}
	mine := stackWorkspace(t, "ghs-mine", "admin", link)
	stackWorkspace(t, "ghs-foreign", "", map[string]any{"github_stack_autolink": map[string]any{"mode": "report"}})

	got := stackStatus(t, mine, "?repo=acme/app")
	if got.FinalMode != "report" {
		t.Fatalf("final_mode = %q, want report (a foreign workspace is in report)", got.FinalMode)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].WorkspaceID != mine || got.Workspaces[0].EffectiveMode != "link" {
		t.Fatalf("workspaces = %+v, want only mine at link", got.Workspaces)
	}
	if got.HiddenWorkspaces != 1 {
		t.Fatalf("hidden_workspaces = %d, want 1", got.HiddenWorkspaces)
	}
}

func TestGitHubStackAutolinkStatusEnvKillAndUnboundOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := stackWorkspace(t, "ghs-env", "member", map[string]any{"github_stack_autolink": map[string]any{"mode": "link"}})

	if got := stackStatus(t, ws, "?repo=nobody/app"); got.FinalMode != "off" || len(got.Workspaces) != 0 {
		t.Fatalf("unbound owner: %+v, want off with no workspaces", got)
	}

	t.Setenv("GITHUB_STACK_AUTOLINK", "0")
	if got := stackStatus(t, ws, "?repo=acme/app"); got.FinalMode != "off" || !got.EnvKill {
		t.Fatalf("env kill: final_mode=%q env_kill=%t, want off true", got.FinalMode, got.EnvKill)
	}
}

func TestGitHubStackAutolinkStatusRejectsBadRepo(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := stackWorkspace(t, "ghs-badrepo", "member", nil)
	for _, q := range []string{"", "?repo=acme", "?repo=/app", "?repo=a/b/c"} {
		testutil.Call(t, testHandler.GetGitHubStackAutolink, stackRequest("GET", "/api/github/stack-autolink"+q, nil, ws)).Want(http.StatusBadRequest)
	}
}

func TestGitHubStackAutolinkSetByAdminAndOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, role := range []string{"admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			ws := stackWorkspace(t, "ghs-set-"+role, role, map[string]any{"keep_me": "yes"})

			var out stackAutolinkSet
			testutil.Call(t, testHandler.SetGitHubStackAutolink, stackRequest("PUT", "/api/github/stack-autolink", map[string]any{"mode": "report"}, ws)).Want(http.StatusOK).JSON(&out)
			if out.WorkspaceID != ws || out.Mode != "report" || out.Repo != "" {
				t.Fatalf("workspace set = %+v", out)
			}

			testutil.Call(t, testHandler.SetGitHubStackAutolink, stackRequest("PUT", "/api/github/stack-autolink", map[string]any{"repo": "Acme/App", "mode": "link"}, ws)).Want(http.StatusOK).JSON(&out)
			if out.Repo != "acme/app" || out.Mode != "link" || out.EffectiveMode != "link" {
				t.Fatalf("repo set = %+v", out)
			}

			settings := storedStackAutolink(t, ws)
			cfg, _ := settings["github_stack_autolink"].(map[string]any)
			repos, _ := cfg["repos"].(map[string]any)
			if cfg["mode"] != "report" || repos["acme/app"] != "link" {
				t.Fatalf("stored config = %v, want mode report and acme/app link", cfg)
			}
			if settings["keep_me"] != "yes" {
				t.Fatalf("other settings were dropped: %v", settings)
			}
			if got := stackStatus(t, ws, "?repo=acme/app"); got.FinalMode != "link" {
				t.Fatalf("status after set = %q, want link", got.FinalMode)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1 AND kind = 'workspace.update'`, ws); n != 2 {
				t.Fatalf("workspace.update audit rows = %d, want 2", n)
			}
		})
	}
}

// A case variant of an existing repo key is replaced, not duplicated.
func TestGitHubStackAutolinkSetReplacesCaseVariantRepoKey(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := stackWorkspace(t, "ghs-case", "owner", map[string]any{"github_stack_autolink": map[string]any{"mode": "report", "repos": map[string]any{"Acme/App": "link"}}})
	testutil.Call(t, testHandler.SetGitHubStackAutolink, stackRequest("PUT", "/api/github/stack-autolink", map[string]any{"repo": "acme/app", "mode": "off"}, ws)).Want(http.StatusOK)
	repos, _ := storedStackAutolink(t, ws)["github_stack_autolink"].(map[string]any)["repos"].(map[string]any)
	if len(repos) != 1 || repos["acme/app"] != "off" {
		t.Fatalf("repos = %v, want only acme/app=off", repos)
	}
}

func TestGitHubStackAutolinkSetRefusedForMember(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := stackWorkspace(t, "ghs-member", "member", map[string]any{})
	testutil.Call(t, testHandler.SetGitHubStackAutolink, stackRequest("PUT", "/api/github/stack-autolink", map[string]any{"mode": "link"}, ws)).Want(http.StatusForbidden)
	if _, written := storedStackAutolink(t, ws)["github_stack_autolink"]; written {
		t.Fatal("a refused set still wrote the setting")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1 AND kind = 'workspace.update'`, ws); n != 0 {
		t.Fatalf("a refused set wrote %d audit rows", n)
	}
}

func TestGitHubStackAutolinkSetRejectsInvalidInput(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ws := stackWorkspace(t, "ghs-invalid", "owner", map[string]any{})
	for name, body := range map[string]any{
		"unknown mode":  map[string]any{"mode": "strict"},
		"missing mode":  map[string]any{"repo": "acme/app"},
		"empty mode":    map[string]any{"mode": ""},
		"non-string":    map[string]any{"mode": 1},
		"repo no slash": map[string]any{"repo": "acme", "mode": "link"},
		"repo 3 parts":  map[string]any{"repo": "a/b/c", "mode": "link"},
	} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.SetGitHubStackAutolink, stackRequest("PUT", "/api/github/stack-autolink", body, ws)).Want(http.StatusBadRequest)
		})
	}
	if _, written := storedStackAutolink(t, ws)["github_stack_autolink"]; written {
		t.Fatal("a rejected set still wrote the setting")
	}
}
