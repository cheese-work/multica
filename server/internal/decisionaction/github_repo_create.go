package decisionaction

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/githubrepo"
)

// GitHubRepoCreate is the action an agent proposes instead of creating a
// repository itself (CHE-9525). The server creates it with an org-admin
// credential no agent holds, then adds it to the guard allowlist and the
// workspace repo registry in the same approval.
const GitHubRepoCreate = "github.repo.create"

// CodeOrgNotAllowed refuses an org that is not an approved agent target.
const CodeOrgNotAllowed = "org_not_allowed"

// GitHubRepoCreateParams is the params of github.repo.create.
type GitHubRepoCreateParams struct {
	WorkspaceID string `json:"workspace_id"`
	Org         string `json:"org"`
	Name        string `json:"name"`
	Visibility  string `json:"visibility"`
	Template    string `json:"template,omitempty"`
	Description string `json:"description,omitempty"`
}

var (
	repoCreateNamePattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	repoCreateTemplatePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
)

const maxRepoDescription = 350

func normalizeGitHubRepoCreate(raw json.RawMessage, slots []SecretSlot) (json.RawMessage, []SecretSlot, error) {
	if len(slots) > 0 {
		return nil, nil, invalid("%s takes no secret slots: the server holds the admin credential", GitHubRepoCreate)
	}
	var params GitHubRepoCreateParams
	if err := decodeStrict(raw, &params); err != nil {
		return nil, nil, err
	}
	var err error
	if params.WorkspaceID, err = canonicalUUID("workspace_id", params.WorkspaceID); err != nil {
		return nil, nil, err
	}
	if !githubrepo.IsApprovedOrg(params.Org) {
		return nil, nil, &Error{Code: CodeOrgNotAllowed, Message: "org is not an approved agent target for repository creation"}
	}
	params.Org = strings.ToLower(params.Org)
	if !repoCreateNamePattern.MatchString(params.Name) || params.Name == "." || params.Name == ".." || strings.HasSuffix(params.Name, ".git") {
		return nil, nil, invalid("name must be 1 to 100 letters, digits, . _ - and not end in .git")
	}
	switch params.Visibility {
	case "private", "internal", "public":
	default:
		return nil, nil, invalid("visibility must be private, internal or public")
	}
	if params.Template != "" {
		owner, _, found := strings.Cut(params.Template, "/")
		if !repoCreateTemplatePattern.MatchString(params.Template) || !found {
			return nil, nil, invalid("template must be an owner/name slug")
		}
		if !githubrepo.IsApprovedOrg(owner) {
			return nil, nil, &Error{Code: CodeOrgNotAllowed, Message: "template must live in an approved org"}
		}
	}
	if utf8.RuneCountInString(params.Description) > maxRepoDescription || strings.ContainsAny(params.Description, "\r\n") {
		return nil, nil, invalid("description must be one line of at most %d characters", maxRepoDescription)
	}
	out, err := json.Marshal(params)
	return out, nil, err
}

func init() {
	Register(&Definition{
		Type: GitHubRepoCreate, Version: 1, Passkey: PasskeyRequired, MinClass: "spend", Target: TargetWorkspace, OwnerOnly: true,
		Template:  Template{Title: "Create a GitHub repository", Summary: "Creates the repository with an org-admin credential, then adds it to the guard allowlist and the workspace repo registry."},
		Normalize: normalizeGitHubRepoCreate,
	})
}
