-- name: IsIssueInRunIssueTree :one
-- CHE-2216 A5a: the run's own issue and its descendants. own_issue_id is NULL
-- for a chat task, which owns no tree.
WITH RECURSIVE own_tree(id) AS (
    SELECT i.id FROM issue i
    WHERE i.workspace_id = sqlc.arg('workspace_id') AND i.id = sqlc.narg('own_issue_id')
    UNION ALL
    SELECT c.id FROM issue c JOIN own_tree t ON c.parent_issue_id = t.id
    WHERE c.workspace_id = sqlc.arg('workspace_id')
)
SELECT EXISTS (
    SELECT 1 FROM issue
    WHERE issue.id = sqlc.arg('issue_id') AND issue.id IN (SELECT id FROM own_tree)
) AS in_tree;

-- name: IsIssueCreatedByTask :one
-- CHE-2216 A5a: issues stamped with this task as their agent_create origin.
SELECT EXISTS (
    SELECT 1 FROM issue
    WHERE issue.id = sqlc.arg('issue_id') AND issue.workspace_id = sqlc.arg('workspace_id')
      AND issue.origin_type = 'agent_create' AND issue.origin_id = sqlc.arg('task_id')
) AS created;
