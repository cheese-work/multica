package service

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var githubReviewTaskKeyPattern = regexp.MustCompile(`(?:^|-)([0-9a-f]{12})(?:-[0-9]+)?$`)

func GitHubPullRequestAuthor(ctx context.Context, queries *db.Queries, pull db.GithubPullRequest) (db.Agent, error) {
	workspace, err := queries.GetWorkspace(ctx, pull.WorkspaceID)
	if err != nil {
		return db.Agent{}, err
	}
	var settings struct {
		Authors map[string]string `json:"github_review_authors"`
	}
	if err := json.Unmarshal(workspace.Settings, &settings); err != nil {
		return db.Agent{}, err
	}
	var authorID pgtype.UUID
	pullKey := fmt.Sprintf("%s/%s#%d", strings.ToLower(pull.RepoOwner), strings.ToLower(pull.RepoName), pull.PrNumber)
	if binding, exists := settings.Authors[pullKey]; exists {
		authorID, err = util.ParseUUID(binding)
		if err != nil {
			return db.Agent{}, pgx.ErrNoRows
		}
	} else {
		if !pull.Branch.Valid || pull.Branch.String == "" {
			return db.Agent{}, pgx.ErrNoRows
		}
		taskKey := ""
		branch := strings.Split(pull.Branch.String, "/")
		if len(branch) == 3 && branch[0] == "agent" && branch[1] != "" {
			if match := githubReviewTaskKeyPattern.FindStringSubmatch(branch[2]); len(match) == 2 {
				taskKey = match[1]
			}
		}
		candidates, err := queries.ListGitHubReviewAuthorCandidates(ctx, db.ListGitHubReviewAuthorCandidatesParams{
			PullRequestID: pull.ID, WorkspaceID: pull.WorkspaceID, TaskKey: taskKey,
		})
		if err != nil {
			return db.Agent{}, err
		}
		if len(candidates) != 1 {
			return db.Agent{}, pgx.ErrNoRows
		}
		authorID = candidates[0]
	}
	return queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: authorID, WorkspaceID: pull.WorkspaceID})
}
