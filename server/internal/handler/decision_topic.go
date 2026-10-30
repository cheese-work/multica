package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxDecisionTopicDepth stops a parent cycle from looping the root walk.
const maxDecisionTopicDepth = 64

// decisionTopic is what one decision DM per person is about. Root is set when
// the topic is an issue; a project topic has no root.
type decisionTopic struct {
	ID    pgtype.UUID
	Title string
	Root  *db.Issue
}

// resolveDecisionTopic is the root of the issue's parent chain. An issue with
// no parent has the project as topic, or itself when it has no project.
func resolveDecisionTopic(ctx context.Context, queries *db.Queries, issue db.Issue) (decisionTopic, error) {
	root := issue
	for depth := 0; root.ParentIssueID.Valid && depth < maxDecisionTopicDepth; depth++ {
		parent, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: root.ParentIssueID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return decisionTopic{}, err
		}
		root = parent
	}
	if root.ID == issue.ID && issue.ProjectID.Valid {
		project, err := queries.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: issue.ProjectID, WorkspaceID: issue.WorkspaceID})
		return decisionTopic{ID: project.ID, Title: project.Title}, err
	}
	return decisionTopic{ID: root.ID, Title: root.Title, Root: &root}, nil
}

// decisionTopicAgent is the agent the root issue is assigned to; a squad
// resolves to its leader. It is the zero value when there is none.
func decisionTopicAgent(ctx context.Context, queries *db.Queries, root db.Issue) (pgtype.UUID, error) {
	switch root.AssigneeType.String {
	case "agent":
		return root.AssigneeID, nil
	case "squad":
		squad, err := queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: root.AssigneeID, WorkspaceID: root.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, nil
		}
		return squad.LeaderID, err
	}
	return pgtype.UUID{}, nil
}

// decisionSteward owns a new topic DM: the root issue's agent, else the asker.
func (handler *Handler) decisionSteward(ctx context.Context, queries *db.Queries, topic decisionTopic, asker db.Agent, humanID pgtype.UUID) (db.Agent, error) {
	if topic.Root == nil {
		return asker, nil
	}
	id, err := decisionTopicAgent(ctx, queries, *topic.Root)
	if err != nil || !id.Valid || id == asker.ID {
		return asker, err
	}
	steward, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: topic.Root.WorkspaceID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return asker, nil
	}
	if err != nil {
		return asker, err
	}
	if steward.ArchivedAt.Valid || steward.Kind == "system" || !handler.canInvokeAgent(ctx, steward, "member", uuidToString(humanID), "", uuidToString(topic.Root.WorkspaceID)) {
		return asker, nil
	}
	return steward, nil
}

// moveDecisionStewards hands a root issue's topic DMs to its new agent
// assignee. The DMs stay; without a usable agent assignee they stay as is.
// followDecisionSteward moves the root issue's decision DMs to its new
// assignee. It is best-effort: the assignee change has already committed.
func (handler *Handler) followDecisionSteward(ctx context.Context, issue db.Issue) {
	if err := handler.moveDecisionStewards(ctx, issue); err != nil {
		slog.Warn("move decision stewards failed", "issue_id", uuidToString(issue.ID), "error", err)
	}
}

func (handler *Handler) moveDecisionStewards(ctx context.Context, issue db.Issue) error {
	id, err := decisionTopicAgent(ctx, handler.Queries, issue)
	if err != nil || !id.Valid {
		return err
	}
	steward, err := handler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: issue.WorkspaceID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (steward.ArchivedAt.Valid || steward.Kind == "system") {
		return nil
	}
	if err != nil {
		return err
	}
	sessions, err := handler.Queries.ListDecisionTopicSessions(ctx, db.ListDecisionTopicSessionsParams{WorkspaceID: issue.WorkspaceID, TopicID: issue.ID, AgentID: steward.ID})
	if err != nil {
		return err
	}
	for _, session := range sessions {
		// A DM whose person cannot invoke the new steward stays with the old one.
		if !handler.canInvokeAgent(ctx, steward, "member", uuidToString(session.CreatorID), "", uuidToString(issue.WorkspaceID)) {
			continue
		}
		if _, err := handler.Queries.MoveDecisionSessionSteward(ctx, db.MoveDecisionSessionStewardParams{WorkspaceID: issue.WorkspaceID, ID: session.ID, AgentID: steward.ID}); err != nil {
			return err
		}
	}
	return nil
}

// topicLane locks and returns the person's DM for the topic; found is false
// when they have none yet. The advisory lock holds until the transaction ends,
// so two asks under one topic cannot both create the DM.
func topicLane(ctx context.Context, queries *db.Queries, issue db.Issue, topic decisionTopic, humanID, agentID pgtype.UUID) (session db.ChatSession, found bool, err error) {
	if err := queries.LockDecisionTopicLane(ctx, db.LockDecisionTopicLaneParams{WorkspaceID: issue.WorkspaceID, CreatorID: humanID, TopicID: topic.ID}); err != nil {
		return db.ChatSession{}, false, err
	}
	id, err := queries.GetDecisionTopicLane(ctx, db.GetDecisionTopicLaneParams{WorkspaceID: issue.WorkspaceID, CreatorID: humanID, TopicID: topic.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ChatSession{}, false, nil
	}
	if err != nil {
		return db.ChatSession{}, false, err
	}
	session, err = lockDecisionLane(ctx, queries, issue, topic, id, agentID)
	return session, err == nil, err
}

// requireLaneInTopic refuses a DM that is not the topic's. A DM from before
// topics has no topic and is judged as it always was: by naming the issue.
func requireLaneInTopic(ctx context.Context, queries *db.Queries, issue db.Issue, topic decisionTopic, session db.ChatSession) error {
	if !session.DecisionTopicID.Valid {
		return requireLaneNamesIssue(ctx, queries, issue, session)
	}
	if session.DecisionTopicID != topic.ID {
		return decisionIntentRefusal{http.StatusForbidden, "DM " + uuidToString(session.ID) + " is the decision lane of another topic: " + uuidToString(session.DecisionTopicID)}
	}
	return nil
}
