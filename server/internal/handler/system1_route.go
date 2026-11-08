package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/system1"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4439 S1: the route label. Every status-changing issue write that carries
// a routeCapture hook (and a statusRecord) stores one system1_observation row
// of kind `route`, in the write's own transaction, and shares a pre-assigned
// event id with the status_changed activity row. Ids and keys only, no text.

type routeCaptureKey struct{}

// routeCapture is the single-use hook an issue write carries to label its route.
type routeCapture struct {
	Via            string // "route" (owner and status together) | "status" (status-only fallback)
	DecisionSource string // "agent" | "human"
	Caller         workflowCaller
	RunStep        string // the run's workflow_step when the request started
	OwnerExplicit  bool
	Confidence     pgtype.Float4
	used           bool
}

func withRouteCapture(ctx context.Context, hook *routeCapture) context.Context {
	return context.WithValue(ctx, routeCaptureKey{}, hook)
}

func routeCaptureFrom(ctx context.Context) *routeCapture {
	hook, _ := ctx.Value(routeCaptureKey{}).(*routeCapture)
	return hook
}

type routingExitInput struct {
	AgentRun          bool
	RunStep, From, To string
}

// routingExit is the one predicate for "this write is a routing agent leaving
// its routing step": an agent run whose step at request start is the locked
// from-status, the write moves the status, and that step has a routing agent.
func routingExit(cfg service.RoutingAgentConfig, scope string, in routingExitInput) bool {
	return in.AgentRun && in.RunStep != "" && in.RunStep == in.From && in.From != in.To &&
		cfg.AgentFor(scope, in.From) != ""
}

type routeRowInput struct {
	WorkspaceID, IssueID, ProjectID pgtype.UUID
	Workflow, From, To, Observed    string
	Priority, FromAssignee          string
	OwnerType                       string
	OwnerID, RouterAgentID, TaskID  pgtype.UUID
	OwnerExplicit                   bool
	Via, DecisionSource             string
	Confidence                      pgtype.Float4
	RoutingExit                     bool
	RecentStatuses                  []string
	EventID                         pgtype.UUID
}

// buildRouteObservation maps one route to its row. Class is
// <workflow>/<from status>; choice_key is <to status>|<owner type>:<owner id>.
func buildRouteObservation(in routeRowInput, precedents []string) db.CreateSystem1ObservationParams {
	workflow := in.Workflow
	if workflow == "" {
		workflow = "none"
	}
	choice := in.To + "|" + in.OwnerType + ":" + util.UUIDToString(in.OwnerID)
	prediction := system1.Predict(precedents)
	recent := in.RecentStatuses
	if recent == nil {
		recent = []string{}
	}
	inputs, _ := json.Marshal(map[string]any{
		"from_status": in.From, "to_status": in.To, "observed_status": in.Observed,
		"workflow": in.Workflow, "project_id": util.UUIDToString(in.ProjectID), "priority": in.Priority,
		"from_assignee": in.FromAssignee, "router_agent_id": util.UUIDToString(in.RouterAgentID),
		"task_id": util.UUIDToString(in.TaskID), "owner_explicit": in.OwnerExplicit, "via": in.Via,
		"routing_exit": in.RoutingExit, "recent_statuses": recent,
	})
	return db.CreateSystem1ObservationParams{
		WorkspaceID: in.WorkspaceID, Kind: "route", SubjectID: in.IssueID, Class: workflow + "/" + in.From,
		DecisionSource:      pgtype.Text{String: in.DecisionSource, Valid: true},
		Inputs:              inputs,
		AgentConfidence:     in.Confidence,
		PredictedKey:        pgtype.Text{String: prediction.Key, Valid: true},
		PredictedConfidence: pgtype.Float4{Float32: float32(prediction.Confidence), Valid: prediction.Key != ""},
		PredictedPrecedents: pgtype.Int4{Int32: int32(prediction.Precedents), Valid: true},
		ChoiceKey:           pgtype.Text{String: choice, Valid: true},
		Outcome:             pgtype.Text{String: "applied", Valid: true},
		EventID:             in.EventID,
	}
}

// captureRouteLabel runs inside the issue write's transaction, after the issue
// row is locked and before the write lands. It needs a statusRecord (no record,
// no label) and uses its hook once. A status-changing write gets a fresh UUIDv7
// that the status_changed row reuses; an owner-only route keeps event_id NULL.
// Strict: an error fails the write. (The fallback that tolerates a failed
// capture belongs to the later slice that arms the hook.)
func captureRouteLabel(ctx context.Context, q *db.Queries, params db.UpdateIssueParams) error {
	hook, rec := routeCaptureFrom(ctx), statusRecordFrom(ctx)
	if hook == nil || hook.used || rec == nil {
		return nil
	}
	prior, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: params.ID, WorkspaceID: rec.workspaceID})
	if err != nil {
		return fmt.Errorf("lock issue for route label: %w", err)
	}
	to := prior.Status
	if params.Status.Valid {
		to = params.Status.String
	}
	statusChanges := to != prior.Status
	if !statusChanges && hook.Via != "route" {
		return nil
	}
	hook.used = true

	var def *issueworkflow.Definition
	var scope string
	if prior.ProjectID.Valid {
		project, err := q.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{ID: prior.ProjectID, WorkspaceID: prior.WorkspaceID})
		if err != nil {
			return fmt.Errorf("read project for route label: %w", err)
		}
		if def, err = issueworkflow.ForProjectRow(ctx, q, project); err != nil {
			return fmt.Errorf("read workflow for route label: %w", err)
		}
		scope = service.WorkflowScope(ctx, q, project)
	}
	cfg, err := loadRoutingAgent(ctx, q, prior.WorkspaceID)
	if err != nil {
		return fmt.Errorf("read routing agent for route label: %w", err)
	}
	exit := routingExit(cfg, scope, routingExitInput{AgentRun: hook.Caller.agent && hook.Caller.run != nil, RunStep: hook.RunStep, From: prior.Status, To: to})

	// The owner is who holds the issue after the write: the one in params, else the current one.
	ownerType, ownerID := prior.AssigneeType.String, prior.AssigneeID
	if params.AssigneeType.Valid || params.AssigneeID.Valid {
		ownerType, ownerID = params.AssigneeType.String, params.AssigneeID
	}
	in := routeRowInput{
		WorkspaceID: prior.WorkspaceID, IssueID: prior.ID, ProjectID: prior.ProjectID,
		From: prior.Status, To: to, Observed: prior.Status, Priority: prior.Priority,
		FromAssignee: prior.AssigneeType.String + ":" + util.UUIDToString(prior.AssigneeID),
		OwnerType:    ownerType, OwnerID: ownerID, RouterAgentID: parseUUIDOrZero(cfg.AgentID),
		OwnerExplicit: hook.OwnerExplicit, Via: hook.Via, DecisionSource: hook.DecisionSource,
		Confidence: hook.Confidence, RoutingExit: exit,
	}
	if def != nil {
		in.Workflow = def.Name
	}
	if hook.Caller.run != nil {
		in.TaskID = hook.Caller.run.ID
	}
	if hook.Via == "status" {
		in.Confidence = pgtype.Float4{}
	}
	if in.RecentStatuses, err = q.ListRecentIssueStatusChanges(ctx, prior.ID); err != nil {
		return fmt.Errorf("read recent statuses for route label: %w", err)
	}
	if statusChanges {
		in.EventID = dbid.NewV7()
		rec.eventID, rec.routingExit = in.EventID, exit
	}
	class := buildRouteObservation(in, nil).Class
	precedents, err := q.ListSystem1PrecedentKeys(ctx, db.ListSystem1PrecedentKeysParams{WorkspaceID: prior.WorkspaceID, Kind: "route", Class: class})
	if err != nil {
		return fmt.Errorf("read route precedents: %w", err)
	}
	if err := q.CreateSystem1Observation(ctx, buildRouteObservation(in, precedents)); err != nil {
		return fmt.Errorf("record route label: %w", err)
	}
	return nil
}
