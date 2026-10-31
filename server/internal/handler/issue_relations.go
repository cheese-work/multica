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
	includeMentions := false
	if values, exists := request.URL.Query()["include"]; exists {
		if len(values) != 1 || values[0] != "mentions" {
			writeError(writer, http.StatusBadRequest, "include must be mentions")
			return
		}
		includeMentions = true
	}
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
	err := handler.DB.QueryRow(ctx, issueRelationsSQL(includeMentions), issue.WorkspaceID, issue.ID, depth, agents, handler.getIssuePrefix(ctx, issue.WorkspaceID)).Scan(&graph)
	if err != nil {
		slog.ErrorContext(ctx, "load issue relations failed", "issue_id", uuidToString(issue.ID), "error", err)
		writeError(writer, http.StatusInternalServerError, "failed to load issue relations")
		return
	}
	writeJSON(writer, http.StatusOK, graph)
}

func issueRelationsSQL(includeMentions bool) string {
	mentionCTE, mentionEdges := "", ""
	if includeMentions {
		mentionCTE = `
forward_mentions AS MATERIALIZED (
 SELECT i.id AS source,lower(ref[1]) AS target
 FROM frontier i
 CROSS JOIN LATERAL regexp_matches(i.description,'mention://issue/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})','g') ref
 UNION ALL
 SELECT i.id,lower(ref[1])
 FROM frontier i JOIN comment c ON c.issue_id=i.id
 CROSS JOIN LATERAL regexp_matches(c.content,'mention://issue/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})','g') ref
), `
		mentionEdges = `
 UNION ALL
 SELECT mention.source,target.id,'mention'
 FROM forward_mentions mention JOIN workspace_issues target ON target.id::text=mention.target
 UNION ALL
 SELECT i.id,target.id,'mention'
 FROM workspace_issues i JOIN frontier target
  ON lower(coalesce(i.description,'')) LIKE '%mention://issue/'||target.id::text||'%'
 UNION ALL
 SELECT i.id,target.id,'mention'
 FROM workspace_issues i JOIN comment c ON c.issue_id=i.id
 JOIN frontier target ON lower(c.content) LIKE '%mention://issue/'||target.id::text||'%'`
	}
	edgesSQL := mentionCTE + `raw_edges AS (
 SELECT parent_issue_id AS source,id AS target,'parent'::text AS type
 FROM frontier WHERE parent_issue_id IS NOT NULL
 UNION ALL
 SELECT i.parent_issue_id,i.id,'parent'
 FROM workspace_issues i JOIN frontier ON i.parent_issue_id=frontier.id
 UNION ALL
 SELECT id,duplicate_of_issue_id,'duplicate' FROM frontier
 WHERE duplicate_of_issue_id IS NOT NULL AND status='cancelled'
 UNION ALL
 SELECT i.id,i.duplicate_of_issue_id,'duplicate'
 FROM workspace_issues i JOIN frontier ON i.duplicate_of_issue_id=frontier.id
 WHERE i.status='cancelled'
 UNION ALL
 SELECT watched.id,w.issue_id,'wakeup'
 FROM issue_wakeup w
 JOIN workspace_issues watched ON watched.id::text=w.condition->>'issue_id'
 WHERE w.workspace_id=$1 AND w.agent_id=ANY($4::uuid[]) AND w.enabled
  AND w.disabled_at IS NULL AND w.condition->>'type'='other_issue'
  AND (watched.id IN (SELECT id FROM frontier) OR w.issue_id IN (SELECT id FROM frontier))
 UNION ALL
 SELECT child.id,w.issue_id,'children_done'
 FROM issue_wakeup w JOIN workspace_issues child ON child.parent_issue_id=w.issue_id
 WHERE w.workspace_id=$1 AND w.agent_id=ANY($4::uuid[]) AND w.enabled
  AND w.disabled_at IS NULL AND w.condition->>'type'='children_done'
  AND (w.condition->>'stage' IS NULL OR child.stage::text=w.condition->>'stage')
  AND (child.id IN (SELECT id FROM frontier) OR w.issue_id IN (SELECT id FROM frontier))
` + mentionEdges + `
 UNION ALL
 SELECT least(first.issue_id,second.issue_id),greatest(first.issue_id,second.issue_id),'shared_pr'
 FROM frontier JOIN issue_pull_request first ON first.issue_id=frontier.id
 JOIN issue_pull_request second
  ON first.pull_request_id=second.pull_request_id AND first.issue_id<>second.issue_id
 JOIN github_pull_request pr ON pr.id=first.pull_request_id AND pr.workspace_id=$1
), edges AS (
 SELECT DISTINCT e.source,e.target,e.type FROM raw_edges e
 JOIN workspace_issues source ON source.id=e.source
 JOIN workspace_issues target ON target.id=e.target
 WHERE e.source<>e.target
)
`

	return `
WITH RECURSIVE
workspace_issues AS NOT MATERIALIZED (
 SELECT id,number,title,status,description,parent_issue_id,duplicate_of_issue_id,stage
 FROM issue WHERE workspace_id=$1
), walk(ids,seen,hops) AS (
 SELECT ARRAY[$2::uuid],ARRAY[$2::uuid],0
 UNION ALL
 SELECT neighbors.ids,walk.seen||neighbors.ids,walk.hops+1
 FROM walk CROSS JOIN LATERAL (
  WITH frontier AS MATERIALIZED (
   SELECT * FROM workspace_issues WHERE id=ANY(walk.ids)
  ), ` + edgesSQL + `
  SELECT coalesce(array_agg(id),'{}'::uuid[]) AS ids
  FROM (SELECT source AS id FROM edges UNION SELECT target FROM edges) candidates
  WHERE NOT (id=ANY(walk.seen))
 ) neighbors
 WHERE walk.hops<$3 AND cardinality(walk.ids)>0
), reachable AS (
 SELECT expanded.id,min(walk.hops) AS hops
 FROM walk CROSS JOIN LATERAL unnest(walk.ids) expanded(id) GROUP BY expanded.id
), selected AS MATERIALIZED (
 SELECT id,hops FROM reachable ORDER BY hops,id LIMIT 40
), selected_issues AS MATERIALIZED (
 SELECT i.* FROM workspace_issues i JOIN selected ON selected.id=i.id
), edges AS MATERIALIZED (
 WITH workspace_issues AS NOT MATERIALIZED (
  SELECT * FROM selected_issues
 ), frontier AS MATERIALIZED (
  SELECT * FROM workspace_issues
 ), ` + edgesSQL + `
 SELECT * FROM edges
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
}
