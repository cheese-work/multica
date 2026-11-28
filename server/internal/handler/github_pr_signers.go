package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

var (
	signersRepoRe = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)$`)
	signersHeadRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
)

// pullRequestSignersResponse is the CHE-10267 contract. The server returns
// facts only; any merge rule lives in the consumer (CHE-10249).
type pullRequestSignersResponse struct {
	Repo    string `json:"repo"`
	PR      int32  `json:"pr"`
	HeadSHA string `json:"head_sha"`
	IssueID string `json:"issue_id"`
	Linked  bool   `json:"linked"`
	service.VerifiedSignersResult
}

// GetPullRequestSigners (GET /api/github/pull-requests/signers) returns the
// agents whose trusted PASS stands on the exact head of a stored pull request,
// the PR author and whether comment protection is on. Members and agents read
// it alike. 400: a bad repo, pr, head or issue. 404: the PR is not stored or
// the issue does not exist. 409: the issue is not linked to the PR.
func (h *Handler) GetPullRequestSigners(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo := q.Get("repo")
	m := signersRepoRe.FindStringSubmatch(repo)
	if m == nil {
		writeError(w, http.StatusBadRequest, "repo must be owner/name")
		return
	}
	number, err := strconv.ParseInt(q.Get("pr"), 10, 32)
	if err != nil || number < 1 {
		writeError(w, http.StatusBadRequest, "pr must be a positive number")
		return
	}
	head := q.Get("head")
	if !signersHeadRe.MatchString(head) {
		writeError(w, http.StatusBadRequest, "head must be a full 40-character commit SHA")
		return
	}
	head = strings.ToLower(head)
	issueRef := q.Get("issue")
	if issueRef == "" {
		writeError(w, http.StatusBadRequest, "issue is required")
		return
	}
	issue, ok := h.loadIssueForUser(w, r, issueRef)
	if !ok {
		return
	}

	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Error("pr signers: begin", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read signers")
		return
	}
	defer tx.Rollback(ctx)
	result, err := service.PullRequestSigners(ctx, tx, h.Queries.WithTx(tx), service.MergeCandidate{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Owner: m[1], Repo: m[2], Number: int32(number), HeadSHA: head,
	}, CommentProtectionOn)
	switch {
	case errors.Is(err, service.ErrSignersPRNotStored):
		writeError(w, http.StatusNotFound, "pull request not found")
		return
	case errors.Is(err, service.ErrSignersIssueNotLinked):
		writeError(w, http.StatusConflict, "issue is not linked to the pull request")
		return
	case err != nil:
		slog.Error("pr signers: read", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read signers")
		return
	}
	writeJSON(w, http.StatusOK, pullRequestSignersResponse{
		Repo: repo, PR: int32(number), HeadSHA: head, IssueID: util.UUIDToString(issue.ID), Linked: true, VerifiedSignersResult: result,
	})
}
