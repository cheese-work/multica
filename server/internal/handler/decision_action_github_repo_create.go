package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/githubrepo"
	"github.com/multica-ai/multica/server/internal/guard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// github.repo.create (CHE-9525): one approval creates the repository, adds it
// to guard_policy.repository_allowlist and to the workspace repo registry.
//
// The writes to Multica rows run first inside the executor's savepoint, and the
// GitHub call runs last: a refusal from GitHub rolls the rows back, so a
// refused or failed create leaves the workspace unchanged. The one gap is a
// commit failure after GitHub answered, which leaves an unlisted repository.

const githubRepoCreateTimeout = 30 * time.Second

// repoCreateChange is what an approval writes to the workspace rows.
type repoCreateChange struct {
	Settings, Repos []byte
	// Allowlist is "unrestricted" when the org has no allowlist entry (a new
	// repository is already reachable), "listed" when it was added or already
	// there.
	Allowlist string
	// Registry is "added" or "present".
	Registry string
	RepoURL  string
}

func githubRepoURL(org, name string) string { return "https://github.com/" + org + "/" + name + ".git" }

// planGitHubRepoCreate computes the settings and repos the approval writes,
// from the workspace row read under the approval lock. It reads and writes
// nothing.
func planGitHubRepoCreate(workspace db.Workspace, params decisionaction.GitHubRepoCreateParams) (repoCreateChange, error) {
	change := repoCreateChange{Settings: workspace.Settings, Repos: workspace.Repos, Allowlist: "unrestricted", Registry: "present", RepoURL: githubRepoURL(params.Org, params.Name)}
	stored, err := decodeStoredSettings(workspace.Settings)
	if err != nil {
		return change, err
	}
	if raw := stored["guard_policy"]; len(raw) > 0 && !isJSONNullRaw(raw) {
		policy := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &policy); err != nil {
			return change, fmt.Errorf("guard_policy is not an object: %w", err)
		}
		allowlist := map[string][]string{}
		if len(policy["repository_allowlist"]) > 0 {
			if err := json.Unmarshal(policy["repository_allowlist"], &allowlist); err != nil {
				return change, fmt.Errorf("repository_allowlist is not an owner map: %w", err)
			}
		}
		for owner, repos := range allowlist {
			if !strings.EqualFold(owner, params.Org) {
				continue
			}
			change.Allowlist = "listed"
			listed := false
			for _, repo := range repos {
				listed = listed || strings.EqualFold(repo, params.Name)
			}
			if !listed {
				allowlist[owner] = append(repos, params.Name)
				if policy["repository_allowlist"], err = json.Marshal(allowlist); err != nil {
					return change, err
				}
				if stored["guard_policy"], err = json.Marshal(policy); err != nil {
					return change, err
				}
				if _, err := guard.ParsePolicy(stored["guard_policy"]); err != nil {
					return change, err
				}
				if change.Settings, err = json.Marshal(stored); err != nil {
					return change, err
				}
			}
			break
		}
	}
	var repos []json.RawMessage
	if len(workspace.Repos) > 0 {
		if err := json.Unmarshal(workspace.Repos, &repos); err != nil {
			return change, fmt.Errorf("workspace repos are not an array: %w", err)
		}
	}
	for _, raw := range repos {
		var ref workspaceRepoRef
		if json.Unmarshal(raw, &ref) == nil && strings.EqualFold(strings.TrimSuffix(ref.URL, ".git"), strings.TrimSuffix(change.RepoURL, ".git")) {
			return change, nil
		}
	}
	entry, err := json.Marshal(workspaceRepoRef{URL: change.RepoURL})
	if err != nil {
		return change, err
	}
	if change.Repos, err = json.Marshal(append(repos, entry)); err != nil {
		return change, err
	}
	change.Registry = "added"
	return change, nil
}

func isJSONNullRaw(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

// executeGitHubRepoCreateAction runs under the workspace row lock the approval holds.
func executeGitHubRepoCreateAction(ctx context.Context, execution decisionActionExecution) (json.RawMessage, error) {
	var params decisionaction.GitHubRepoCreateParams
	if err := json.Unmarshal(execution.Action.Params, &params); err != nil {
		return nil, err
	}
	// The allow table can change between request and approval.
	if !githubrepo.IsApprovedOrg(params.Org) {
		return nil, actionFailure{Code: decisionaction.CodeOrgNotAllowed, Message: "org is not an approved agent target for repository creation"}
	}
	if execution.GitHubRepos == nil {
		return nil, actionFailure{Code: "github_not_configured", Message: "this server has no GitHub repository creator configured"}
	}
	change, err := planGitHubRepoCreate(execution.Workspace, params)
	if err != nil {
		return nil, actionFailure{Code: "workspace_state_invalid", Message: err.Error()}
	}
	if _, err := execution.Queries.UpdateWorkspace(ctx, db.UpdateWorkspaceParams{ID: execution.Workspace.ID, Settings: change.Settings, Repos: change.Repos}); err != nil {
		return nil, err
	}
	createCtx, cancel := context.WithTimeout(ctx, githubRepoCreateTimeout)
	defer cancel()
	created, err := execution.GitHubRepos.CreateRepo(createCtx, githubrepo.Request{Org: params.Org, Name: params.Name, Visibility: params.Visibility, Template: params.Template, Description: params.Description})
	switch {
	case errors.Is(err, githubrepo.ErrNotConfigured):
		return nil, actionFailure{Code: "credential_not_configured", Message: err.Error()}
	case errors.Is(err, githubrepo.ErrExists):
		return nil, actionFailure{Code: "repo_exists", Message: err.Error()}
	case errors.Is(err, githubrepo.ErrTemplate):
		return nil, actionFailure{Code: "template_unavailable", Message: err.Error()}
	case err != nil:
		slog.Warn("github.repo.create failed", "org", params.Org, "name", params.Name, "error", err)
		return nil, actionFailure{Code: "github_create_failed", Message: err.Error()}
	}
	return json.Marshal(map[string]any{
		"org": params.Org, "name": params.Name, "full_name": created.FullName, "url": created.HTMLURL, "visibility": created.Visibility,
		"allowlist": change.Allowlist, "registry": change.Registry,
	})
}

// githubRepoCreatePreview shows what the approval creates and which workspace
// lists it joins. The values come from the stored params and the live workspace.
func (handler *Handler) githubRepoCreatePreview(ctx context.Context, action db.DecisionAction) ([]ActionPreviewField, error) {
	var params decisionaction.GitHubRepoCreateParams
	if err := json.Unmarshal(action.Params, &params); err != nil {
		return nil, err
	}
	workspace, err := handler.Queries.GetWorkspace(ctx, action.WorkspaceID)
	if err != nil {
		return nil, err
	}
	change, err := planGitHubRepoCreate(workspace, params)
	if err != nil {
		return nil, previewUnavailableError{"The workspace settings or repos cannot take this change: " + err.Error()}
	}
	template := params.Template
	if template == "" {
		template = "none (empty repository)"
	}
	allowlistBefore, allowlistAfter := "not restricted for this org", "not restricted for this org"
	if change.Allowlist == "listed" {
		allowlistBefore, allowlistAfter = "listed", "listed"
		if string(change.Settings) != string(workspace.Settings) {
			allowlistBefore = "not listed"
		}
	}
	registryBefore := "listed"
	if change.Registry == "added" {
		registryBefore = "not listed"
	}
	return []ActionPreviewField{
		{"Repository", "does not exist", params.Org + "/" + params.Name},
		{"Visibility", "", params.Visibility},
		{"Template", "", template},
		{"Guard allowlist", allowlistBefore, allowlistAfter},
		{"Workspace repo registry", registryBefore, "listed"},
	}, nil
}

// refuseRepoCreateClass is the request-time rule that a public repository is a
// publication: the proposer must say so with class publish.
func refuseRepoCreateClass(draft *decisionActionDraft, class string) error {
	if draft.Type != decisionaction.GitHubRepoCreate || class == "publish" {
		return nil
	}
	var params decisionaction.GitHubRepoCreateParams
	if err := json.Unmarshal(draft.Params, &params); err != nil {
		return err
	}
	if params.Visibility == "public" {
		return decisionIntentRefusal{http.StatusBadRequest, "publish_class_required: a public repository is a publication; ask with class publish"}
	}
	return nil
}
