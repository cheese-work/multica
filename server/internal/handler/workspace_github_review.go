package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	githubReviewUserKeyPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
	githubReviewPullKeyPattern = regexp.MustCompile(`^[a-z0-9._-]+/[a-z0-9._-]+#[0-9]+$`)
)

// reviewMappingError is a client mistake in a github_review_* body.
type reviewMappingError string

func (e reviewMappingError) Error() string { return string(e) }

// validateGitHubReviewMapping checks one github_review_* object and returns it
// ready to store. members maps a numeric GitHub user id to a workspace member;
// authors maps a lowercase "owner/repo#N" to a workspace agent. The readers
// (canRoute, githubIntakeAuthor, intake) treat a bad entry as "no mapping", so
// a typo would fail silently; reject it here instead.
func (h *Handler) validateGitHubReviewMapping(ctx context.Context, workspaceID pgtype.UUID, field string, raw json.RawMessage) ([]byte, error) {
	var entries map[string]string
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return nil, reviewMappingError(field + " must be an object of strings")
	}
	members := field == "github_review_members"
	clean := make(map[string]string, len(entries))
	for key, value := range entries {
		if !members {
			key = strings.ToLower(key)
		}
		if members && !githubReviewUserKeyPattern.MatchString(key) {
			return nil, reviewMappingError(fmt.Sprintf("%s: %q is not a numeric GitHub user id", field, key))
		}
		if !members && !githubReviewPullKeyPattern.MatchString(key) {
			return nil, reviewMappingError(fmt.Sprintf("%s: %q is not owner/repo#number", field, key))
		}
		id, err := util.ParseUUID(value)
		if err != nil {
			return nil, reviewMappingError(fmt.Sprintf("%s: %q is not an id", field, value))
		}
		if members {
			_, err = h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: id, WorkspaceID: workspaceID})
		} else {
			_, err = h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, reviewMappingError(fmt.Sprintf("%s: %s is not in this workspace", field, value))
		}
		if err != nil {
			return nil, err
		}
		clean[key] = uuidToString(id)
	}
	return json.Marshal(clean)
}
