package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (handler *Handler) GetIssueRelations(writer http.ResponseWriter, request *http.Request) {
	depth := 1
	if values, exists := request.URL.Query()["depth"]; exists {
		if len(values) != 1 || (values[0] != "1" && values[0] != "2") {
			writeError(writer, http.StatusBadRequest, "depth must be 1 or 2")
			return
		}
		if values[0] == "2" {
			depth = 2
		}
	}
	issue, ok := handler.loadIssueForUser(writer, request, chi.URLParam(request, "id"))
	if !ok {
		return
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	member, ok := handler.workspaceMember(writer, request, workspaceID)
	if !ok {
		return
	}
	actorType, actorID := handler.resolveActor(request, requestUserID(request), workspaceID)
	allowed, ok := handler.accessibleAgentIDs(request.Context(), workspaceID, actorType, actorID, member.Role)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "failed to resolve agent access")
		return
	}
	agents := make([]pgtype.UUID, 0, len(allowed))
	for id := range allowed {
		agents = append(agents, parseUUID(id))
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	var graph json.RawMessage
	err := handler.DB.QueryRow(ctx, issueRelationsSQL, issue.WorkspaceID, issue.ID, depth, agents, handler.getIssuePrefix(ctx, issue.WorkspaceID)).Scan(&graph)
	if err != nil {
		slog.ErrorContext(ctx, "load issue relations failed", "issue_id", uuidToString(issue.ID), "error", err)
		writeError(writer, http.StatusInternalServerError, "failed to load issue relations")
		return
	}
	writeJSON(writer, http.StatusOK, graph)
}

const issueRelationsSQL = `
WITH RECURSIVE
workspace_issues AS NOT MATERIALIZED (
 SELECT id,number,title,status,description,parent_issue_id,duplicate_of_issue_id,stage
 FROM issue WHERE workspace_id=$1
), raw_edges AS (
 SELECT parent_issue_id AS source,id AS target,'parent'::text AS type
 FROM workspace_issues WHERE parent_issue_id IS NOT NULL
 UNION ALL
 SELECT id,duplicate_of_issue_id,'duplicate' FROM workspace_issues
 WHERE duplicate_of_issue_id IS NOT NULL AND status='cancelled'
 UNION ALL
 SELECT watched.id,w.issue_id,'wakeup'
 FROM issue_wakeup w
 JOIN workspace_issues watched ON watched.id::text=w.condition->>'issue_id'
 WHERE w.workspace_id=$1 AND w.agent_id=ANY($4::uuid[]) AND w.enabled
  AND w.disabled_at IS NULL AND w.condition->>'type'='other_issue'
 UNION ALL
 SELECT child.id,w.issue_id,'children_done'
 FROM issue_wakeup w JOIN workspace_issues child ON child.parent_issue_id=w.issue_id
 WHERE w.workspace_id=$1 AND w.agent_id=ANY($4::uuid[]) AND w.enabled
  AND w.disabled_at IS NULL AND w.condition->>'type'='children_done'
  AND (w.condition->>'stage' IS NULL OR child.stage::text=w.condition->>'stage')
 UNION ALL
 SELECT i.id,target.id,'mention'
 FROM workspace_issues i
 CROSS JOIN LATERAL regexp_matches(i.description,'mention://issue/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})','g') ref
 JOIN workspace_issues target ON target.id::text=lower(ref[1])
 UNION ALL
 SELECT i.id,target.id,'mention'
 FROM workspace_issues i JOIN comment c ON c.issue_id=i.id
 CROSS JOIN LATERAL regexp_matches(c.content,'mention://issue/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})','g') ref
 JOIN workspace_issues target ON target.id::text=lower(ref[1])
 UNION ALL
 SELECT first.issue_id,second.issue_id,'shared_pr'
 FROM issue_pull_request first JOIN issue_pull_request second
  ON first.pull_request_id=second.pull_request_id AND first.issue_id<second.issue_id
 JOIN github_pull_request pr ON pr.id=first.pull_request_id AND pr.workspace_id=$1
), edges AS MATERIALIZED (
 SELECT DISTINCT e.source,e.target,e.type FROM raw_edges e
 JOIN workspace_issues source ON source.id=e.source
 JOIN workspace_issues target ON target.id=e.target
 WHERE e.source<>e.target
), walk(id,hops) AS (
 SELECT $2::uuid,0
 UNION
 SELECT CASE WHEN e.source=walk.id THEN e.target ELSE e.source END,walk.hops+1
 FROM walk JOIN edges e ON e.source=walk.id OR e.target=walk.id
 WHERE walk.hops<$3
), reachable AS (
 SELECT id,min(hops) AS hops FROM walk GROUP BY id
), selected AS MATERIALIZED (
 SELECT id,hops FROM reachable ORDER BY hops,id LIMIT 40
)
SELECT jsonb_build_object(
 'nodes',(SELECT coalesce(jsonb_agg(jsonb_build_object(
  'id',i.id,'identifier',$5::text||'-'||i.number,'title',i.title,'status',i.status
 ) ORDER BY s.hops,i.id),'[]'::jsonb) FROM selected s JOIN workspace_issues i ON i.id=s.id),
 'edges',(SELECT coalesce(jsonb_agg(jsonb_build_object(
  'source',e.source,'target',e.target,'type',e.type
 ) ORDER BY e.type,e.source,e.target),'[]'::jsonb) FROM edges e
  JOIN selected source ON source.id=e.source JOIN selected target ON target.id=e.target),
 'omitted_count',(SELECT count(*) FROM reachable)-(SELECT count(*) FROM selected),
 'depth',$3::int
)`
