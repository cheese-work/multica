package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/threadrel"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// threadViewSettingKey is the member_setting key holding a member's thread view
// preferences (plan CHE-6385 section 6). S3a reads only fyi_counts_as_with_me.
const threadViewSettingKey = "thread_view"

// HeaderThreadRelation reports on a comment list whether the thread relation
// was left out. It is set to "unavailable" when the relation read failed and
// the comments were returned unfiltered and without thread_relation.
const HeaderThreadRelation = "X-Multica-Thread-Relation"

// ThreadRelationResponse is one thread's relation to the viewer (see threadrel).
// It is set on thread roots only.
type ThreadRelationResponse struct {
	WithMe      bool `json:"with_me"`
	AgentsOnly  bool `json:"agents_only"`
	WaitingOnMe bool `json:"waiting_on_me"`
}

type threadRelationQuery struct {
	Issue  db.Issue
	Viewer threadrel.Actor
}

// threadRelationSet holds the relation per thread root and the root of every
// comment of the issue, so a partial read (since, tail) still maps to a root.
type threadRelationSet struct {
	ByRoot map[string]ThreadRelationResponse
	RootOf map[string]string
}

// relationFilter is the parsed relation= parameter.
type relationFilter string

const (
	relationNone    relationFilter = ""
	relationWithMe  relationFilter = "with_me"
	relationWithout relationFilter = "without_me"
	relationWaiting relationFilter = "waiting"
)

func (f relationFilter) keeps(r ThreadRelationResponse) bool {
	switch f {
	case relationWithMe:
		return r.WithMe
	case relationWithout:
		return !r.WithMe
	case relationWaiting:
		return r.WaitingOnMe
	}
	return true
}

// parseRelationFilter accepts with_me, without_me and waiting. "1" and "true"
// ask for the relation field without a filter (the thread refetch of plan U3).
func parseRelationFilter(s string) (relationFilter, bool) {
	switch s {
	case "", "1", "true":
		return relationNone, true
	case "with_me", "without_me", "waiting":
		return relationFilter(s), true
	}
	return relationNone, false
}

// resolveRelationViewer returns the viewer whose relation the list reports: the
// caller, or the member or agent named by for=. It writes the error response
// itself and reports ok=false.
func (h *Handler) resolveRelationViewer(w http.ResponseWriter, r *http.Request, issue db.Issue, forID string) (threadrel.Actor, bool) {
	if forID == "" {
		t, id := h.resolveActor(r, requestUserID(r), uuidToString(issue.WorkspaceID))
		return threadrel.Actor{Type: t, ID: id}, true
	}
	id, err := parseUUIDStrict(forID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid for parameter; expected a member or agent id")
		return threadrel.Actor{}, false
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: id, WorkspaceID: issue.WorkspaceID}); err == nil {
		return threadrel.Actor{Type: threadrel.ActorMember, ID: uuidToString(id)}, true
	}
	if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID}); err == nil {
		return threadrel.Actor{Type: threadrel.ActorAgent, ID: uuidToString(id)}, true
	}
	writeError(w, http.StatusBadRequest, "for must be a member or an agent of this workspace")
	return threadrel.Actor{}, false
}

// threadRelations computes the relation of every thread of the issue to the
// viewer: one read of the issue's comments, one of its decision requests, and
// one member_setting read for a member viewer.
func (h *Handler) threadRelations(ctx context.Context, q threadRelationQuery) (threadRelationSet, error) {
	if h.threadRelationsForTest != nil {
		return h.threadRelationsForTest(ctx, q)
	}
	comments, err := h.Queries.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{
		IssueID:     q.Issue.ID,
		WorkspaceID: q.Issue.WorkspaceID,
		Limit:       commentProbeLimit(),
	})
	if err != nil {
		return threadRelationSet{}, err
	}
	if len(comments) > commentHardCap {
		return threadRelationSet{}, errors.New("issue has more comments than the relation read covers")
	}
	requests, err := h.Queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{
		WorkspaceID: q.Issue.WorkspaceID,
		IssueID:     q.Issue.ID,
	})
	if err != nil {
		return threadRelationSet{}, err
	}
	fyi := true
	if q.Viewer.Type == threadrel.ActorMember {
		userID, err := parseUUIDStrict(q.Viewer.ID)
		if err != nil {
			return threadRelationSet{}, err
		}
		row, err := h.Queries.GetMemberSetting(ctx, db.GetMemberSettingParams{WorkspaceID: q.Issue.WorkspaceID, UserID: userID, Key: threadViewSettingKey})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return threadRelationSet{}, err
		default:
			var v struct {
				FYICountsAsWithMe *bool `json:"fyi_counts_as_with_me"`
			}
			// A malformed stored value keeps the default.
			if json.Unmarshal(row.Value, &v) == nil && v.FYICountsAsWithMe != nil {
				fyi = *v.FYICountsAsWithMe
			}
		}
	}

	parent := make(map[string]string, len(comments))
	for _, c := range comments {
		if c.ParentID.Valid {
			parent[uuidToString(c.ID)] = uuidToString(c.ParentID)
		}
	}
	rootOf := make(map[string]string, len(comments))
	var find func(id string, depth int) string
	find = func(id string, depth int) string {
		if root, ok := rootOf[id]; ok {
			return root
		}
		root := id
		if p, ok := parent[id]; ok && depth < len(comments) {
			root = find(p, depth+1)
		}
		rootOf[id] = root
		return root
	}

	inputs := map[string]*threadrel.Input{}
	for _, c := range comments {
		id := uuidToString(c.ID)
		root := find(id, 0)
		in := inputs[root]
		if in == nil {
			in = &threadrel.Input{Viewer: q.Viewer, FYICountsAsWithMe: fyi}
			inputs[root] = in
		}
		tc := threadrel.Comment{
			Author:         threadrel.Actor{Type: c.AuthorType, ID: uuidToString(c.AuthorID)},
			Content:        c.Content,
			CreatedAt:      c.CreatedAt.Time,
			ResolvesThread: c.ResolvedAt.Valid,
			Deleted:        c.DeletedAt.Valid,
		}
		if c.NotifyOnly.Valid {
			v := c.NotifyOnly.Bool
			tc.NotifyOnly = &v
		}
		if c.DecisionRequestID.Valid {
			tc.DecisionRequestID = uuidToString(c.DecisionRequestID)
		}
		in.Comments = append(in.Comments, tc)
		if c.ResolvedAt.Valid && !c.DeletedAt.Valid {
			if t := c.ResolvedAt.Time; in.ResolvedAt == nil || t.After(*in.ResolvedAt) {
				at := t
				in.ResolvedAt = &at
			}
		}
	}
	for _, d := range requests {
		if !d.IssueCommentID.Valid {
			continue
		}
		in := inputs[find(uuidToString(d.IssueCommentID), 0)]
		if in == nil {
			continue
		}
		in.Decisions = append(in.Decisions, threadrel.Decision{
			ID:     uuidToString(d.ID),
			Target: threadrel.Actor{Type: threadrel.ActorMember, ID: uuidToString(d.HumanUserID)},
			Status: d.Status,
		})
	}

	out := threadRelationSet{ByRoot: make(map[string]ThreadRelationResponse, len(inputs)), RootOf: rootOf}
	for root, in := range inputs {
		res := threadrel.Classify(*in)
		out.ByRoot[root] = ThreadRelationResponse{WithMe: res.WithMe, AgentsOnly: res.AgentsOnly, WaitingOnMe: res.WaitingOnMe}
	}
	return out, nil
}

// applyThreadRelation filters the fetched comments to the threads the filter
// keeps. It returns the relation set for the response, or nil when the read
// failed: the comments then stay unfiltered and carry no thread_relation.
func (h *Handler) applyThreadRelation(ctx context.Context, q threadRelationQuery, filter relationFilter, comments []db.Comment) ([]db.Comment, *threadRelationSet) {
	started := time.Now()
	set, err := h.threadRelations(ctx, q)
	if err != nil {
		slog.Info("thread relation read failed; returning comments without thread_relation",
			"issue_id", uuidToString(q.Issue.ID), "error", err, "elapsed_ms", time.Since(started).Milliseconds())
		return comments, nil
	}
	if filter == relationNone {
		return comments, &set
	}
	kept := make([]db.Comment, 0, len(comments))
	for _, c := range comments {
		id := uuidToString(c.ID)
		root, ok := set.RootOf[id]
		if !ok {
			root = id
		}
		if rel, ok := set.ByRoot[root]; ok && filter.keeps(rel) {
			kept = append(kept, c)
		}
	}
	return kept, &set
}
