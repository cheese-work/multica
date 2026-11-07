package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
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
	agents, ok := handler.relationAgents(writer, request, workspaceID)
	if !ok {
		return
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

// relationAgents applies the relations access boundary: workspace membership
// plus private-agent visibility. It writes the error response on failure.
func (handler *Handler) relationAgents(writer http.ResponseWriter, request *http.Request, workspaceID string) ([]pgtype.UUID, bool) {
	member, ok := handler.workspaceMember(writer, request, workspaceID)
	if !ok {
		return nil, false
	}
	actorType, actorID := handler.resolveActor(request, requestUserID(request), workspaceID)
	allowed, ok := handler.accessibleAgentIDs(request.Context(), workspaceID, actorType, actorID, member.Role)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "failed to resolve agent access")
		return nil, false
	}
	agents := make([]pgtype.UUID, 0, len(allowed))
	for id := range allowed {
		agents = append(agents, parseUUID(id))
	}
	return agents, true
}

// relationEdgesSQL is the CTE chain that yields `edges` over `frontier` and
// `workspace_issues`; both the walk and the batch endpoint build on it.
func relationEdgesSQL(includeMentions bool) string {
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
	return mentionCTE + `raw_edges AS (
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
 SELECT d.issue_id,d.depends_on_issue_id,'dependency'
 FROM issue_dependency d
 WHERE d.issue_id IN (SELECT id FROM frontier) OR d.depends_on_issue_id IN (SELECT id FROM frontier)
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
}

func issueRelationsSQL(includeMentions bool) string {
	edgesSQL := relationEdgesSQL(includeMentions)
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

const (
	relationsBatchMaxIDs      = 200
	relationsBatchDefaultPage = 1000
	relationsBatchMaxPage     = 2000
)

var relationEdgeTypes = map[string]bool{
	"parent": true, "duplicate": true, "dependency": true, "wakeup": true,
	"children_done": true, "mention": true, "shared_pr": true,
}

type relationsBatchRequest struct {
	IssueIDs        []string `json:"issue_ids"`
	Types           []string `json:"types"`
	IncludeMentions bool     `json:"include_mentions"`
	Page            struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	} `json:"page"`
}

// GetIssueRelationsBatch returns every edge touching issue_ids, keyset-paged on
// (type, source, target). There is no walk and no node cap: a response is the
// last page or carries next_cursor.
func (handler *Handler) GetIssueRelationsBatch(writer http.ResponseWriter, request *http.Request) {
	var body relationsBatchRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.IssueIDs) == 0 || len(body.IssueIDs) > relationsBatchMaxIDs {
		writeError(writer, http.StatusBadRequest, "issue_ids must hold 1 to 200 ids")
		return
	}
	ids, ok := parseUUIDSliceOrBadRequest(writer, body.IssueIDs, "issue_ids")
	if !ok {
		return
	}
	types := body.Types
	if len(types) == 0 {
		for name := range relationEdgeTypes {
			types = append(types, name)
		}
	}
	for _, name := range types {
		if !relationEdgeTypes[name] {
			writeError(writer, http.StatusBadRequest, "unknown edge type")
			return
		}
	}
	limit := body.Page.Limit
	if limit == 0 {
		limit = relationsBatchDefaultPage
	}
	if limit < 0 || limit > relationsBatchMaxPage {
		writeError(writer, http.StatusBadRequest, "page.limit must be 1 to 2000")
		return
	}
	var cursorType string
	var cursorSource, cursorTarget pgtype.UUID
	if body.Page.Cursor != "" {
		parts := strings.Split(body.Page.Cursor, "|")
		var sourceErr, targetErr error
		if len(parts) == 3 {
			cursorSource, sourceErr = util.ParseUUID(parts[1])
			cursorTarget, targetErr = util.ParseUUID(parts[2])
		}
		if len(parts) != 3 || !relationEdgeTypes[parts[0]] || sourceErr != nil || targetErr != nil {
			writeError(writer, http.StatusBadRequest, "invalid page.cursor")
			return
		}
		cursorType = parts[0]
	}
	workspaceID := handler.resolveWorkspaceID(request)
	workspaceUUID, ok := parseUUIDOrBadRequest(writer, workspaceID, "workspace_id")
	if !ok {
		return
	}
	agents, ok := handler.relationAgents(writer, request, workspaceID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	var inWorkspace int
	err := handler.DB.QueryRow(ctx, `SELECT count(*) FROM issue WHERE workspace_id=$1 AND id=ANY($2::uuid[])`, workspaceUUID, ids).Scan(&inWorkspace)
	if err != nil {
		slog.ErrorContext(ctx, "check relations batch ids failed", "error", err)
		writeError(writer, http.StatusInternalServerError, "failed to load issue relations")
		return
	}
	if inWorkspace != len(uniqueUUIDs(ids)) {
		writeError(writer, http.StatusNotFound, "issue not found")
		return
	}
	var cursor any
	if cursorType != "" {
		cursor = cursorType
	}
	var graph json.RawMessage
	err = handler.DB.QueryRow(ctx, issueRelationsBatchSQL(body.IncludeMentions), workspaceUUID, ids, types, agents,
		handler.getIssuePrefix(ctx, workspaceUUID), cursor, cursorSource, cursorTarget, limit).Scan(&graph)
	if err != nil {
		slog.ErrorContext(ctx, "load relations batch failed", "error", err)
		writeError(writer, http.StatusInternalServerError, "failed to load issue relations")
		return
	}
	writeJSON(writer, http.StatusOK, graph)
}

func uniqueUUIDs(ids []pgtype.UUID) map[pgtype.UUID]struct{} {
	set := make(map[pgtype.UUID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// issueRelationsBatchSQL reuses the shared edge CTE with frontier = $2.
// $1 workspace, $2 ids, $3 types, $4 agents, $5 prefix, $6-$8 cursor, $9 limit.
func issueRelationsBatchSQL(includeMentions bool) string {
	return `
WITH workspace_issues AS NOT MATERIALIZED (
 SELECT id,number,title,status,description,parent_issue_id,duplicate_of_issue_id,stage
 FROM issue WHERE workspace_id=$1
), frontier AS MATERIALIZED (
 SELECT * FROM workspace_issues WHERE id=ANY($2::uuid[])
), ` + relationEdgesSQL(includeMentions) + `, fetched AS MATERIALIZED (
 SELECT * FROM edges
 WHERE type=ANY($3::text[]) AND ($6::text IS NULL OR (type,source,target)>($6::text,$7::uuid,$8::uuid))
 ORDER BY type,source,target LIMIT $9+1
), page AS MATERIALIZED (
 SELECT * FROM fetched ORDER BY type,source,target LIMIT $9
), ends AS (
 SELECT source AS id FROM page UNION SELECT target FROM page
)
SELECT jsonb_build_object(
 'nodes',(SELECT coalesce(jsonb_agg(jsonb_build_object(
  'id',i.id,'identifier',$5::text||'-'||i.number,'title',i.title,'status',i.status,
  'parent_issue_id',(SELECT p.id FROM workspace_issues p WHERE p.id=i.parent_issue_id),'context',NOT (i.id=ANY($2::uuid[]))
 ) ORDER BY i.id),'[]'::jsonb) FROM ends JOIN workspace_issues i ON i.id=ends.id),
 'edges',(SELECT coalesce(jsonb_agg(jsonb_build_object(
  'source',e.source,'target',e.target,'type',e.type
 ) ORDER BY e.type,e.source,e.target),'[]'::jsonb) FROM page e),
 'next_cursor',(SELECT CASE WHEN (SELECT count(*) FROM fetched)>$9 THEN
  (SELECT type||'|'||source||'|'||target FROM page ORDER BY type DESC,source DESC,target DESC LIMIT 1) END)
)`
}
