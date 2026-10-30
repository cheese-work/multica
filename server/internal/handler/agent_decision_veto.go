package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const maxAgentDecisionVetoReason = 4096

// Veto is the only way to stop a decision once it may be effective. The caller
// holds the instance lock, so a veto, the quorum settlement and a reliance are
// serialized: whichever commits first, the end state is one revoked settlement
// and every reliance that got in is listed in an undo assignment.
func authorizeAgentDecisionVeto(ctx context.Context, tx pgx.Tx, params db.CreateCommentParams, annotation *agentDecisionAnnotation, scope agentDecisionScope, agentID string, ownerVeto bool) error {
	annotation.vetoReason = agentDecisionVetoReason(params.Content)
	if annotation.vetoReason == "" || len(annotation.vetoReason) > maxAgentDecisionVetoReason {
		return agentDecisionRefusal("a veto needs a reason of at most 4096 bytes")
	}
	var known bool
	owner := annotation.line.undoOwner
	if err := tx.QueryRow(ctx, `SELECT CASE $3
	    WHEN 'agent' THEN EXISTS(SELECT 1 FROM agent WHERE id=$2::uuid AND workspace_id=$1 AND archived_at IS NULL)
	    ELSE EXISTS(SELECT 1 FROM member WHERE user_id=$2::uuid AND workspace_id=$1) END`,
		params.WorkspaceID, owner.id, owner.kind).Scan(&known); err != nil {
		return err
	}
	if !known {
		return agentDecisionRefusal("the undo owner is not an active " + owner.kind + " of this workspace")
	}
	var allowed bool
	var err error
	if ownerVeto {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM member WHERE workspace_id=$1 AND user_id=$2 AND role='owner')`, params.WorkspaceID, params.AuthorID).Scan(&allowed)
	} else {
		named := false
		for _, signer := range scope.Signers {
			named = named || signer.AgentID == agentID
		}
		// The assigned shape of an ordinary decision veto: the assignee agent, or
		// the leader or a member of the assignee squad.
		err = tx.QueryRow(ctx, `SELECT $4 OR EXISTS(SELECT 1 FROM issue i WHERE i.id=$1 AND i.workspace_id=$2 AND (
	    (i.assignee_type='agent' AND i.assignee_id=$3::uuid) OR
	    (i.assignee_type='squad' AND EXISTS(SELECT 1 FROM squad s WHERE s.id=i.assignee_id AND s.workspace_id=i.workspace_id AND s.archived_at IS NULL
	        AND (s.leader_id=$3::uuid OR EXISTS(SELECT 1 FROM squad_member m WHERE m.squad_id=s.id AND m.member_type='agent' AND m.member_id=$3::uuid))))))`,
			params.IssueID, params.WorkspaceID, agentID, named).Scan(&allowed)
	}
	if err != nil {
		return err
	}
	if !allowed {
		return decisionIntentRefusal{403, "agent decision: only an assigned agent, a named signer or a workspace owner may veto"}
	}
	return nil
}

// Everything after the protocol line is the reason; the line itself is not.
func agentDecisionVetoReason(content string) string {
	var reason []string
	skipped := false
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		if !skipped && strings.HasPrefix(strings.TrimSpace(raw), "Veto:") {
			skipped = true
			continue
		}
		reason = append(reason, raw)
	}
	return strings.TrimSpace(strings.Join(reason, "\n"))
}

func revokeAgentDecisionForVeto(ctx context.Context, tx pgx.Tx, commentID pgtype.UUID, annotation *agentDecisionAnnotation) error {
	var workspaceID, issueID, authorID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id,issue_id,author_id FROM comment WHERE id=$1`, commentID).Scan(&workspaceID, &issueID, &authorID); err != nil {
		return err
	}
	return revokeAgentDecision(ctx, tx, workspaceID, issueID, annotation.line.instance, authorID, annotation.line.undoOwner, annotation.vetoReason)
}

type agentDecisionRelianceRef struct {
	RelianceID string `json:"reliance_id"`
	TaskID     string `json:"task_id"`
	AgentID    string `json:"agent_id"`
	Purpose    string `json:"purpose"`
}

// Idempotent and one-way. With no settlement yet it leaves a revoked tombstone,
// so a later quorum insert hits the unique key and stays ineffective. With an
// effective settlement it revokes in place. The call that revokes queues one
// undo assignment to the named owner, listing every reliance that got in (none
// is a valid list) and the evidence to reverify. The instance lock keeps a
// later reliance out, so the list is complete.
func revokeAgentDecision(ctx context.Context, tx pgx.Tx, workspaceID, issueID pgtype.UUID, instance string, by pgtype.UUID, owner agentDecisionUndoOwner, reason string) error {
	if err := lockAgentDecision(ctx, tx, instance); err != nil {
		return err
	}
	var revoked bool
	err := tx.QueryRow(ctx, `INSERT INTO decision_agent_settlement(instance_id,workspace_id,issue_id,revoked_at,revoked_by,revoke_reason,undo_owner_type,undo_owner_id)
        VALUES($1,$2,$3,clock_timestamp(),$4,$5,$6,$7::uuid)
        ON CONFLICT(instance_id) DO UPDATE SET revoked_at=clock_timestamp(),revoked_by=EXCLUDED.revoked_by,revoke_reason=EXCLUDED.revoke_reason,
            undo_owner_type=EXCLUDED.undo_owner_type,undo_owner_id=EXCLUDED.undo_owner_id
        WHERE decision_agent_settlement.revoked_at IS NULL RETURNING true`, instance, workspaceID, issueID, by, reason, owner.kind, owner.id).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already revoked: its owner already holds the assignment
	}
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `INSERT INTO decision_agent_undo(reliance_id,instance_id,workspace_id,issue_id,task_id,agent_id,purpose,reason)
        SELECT r.id,r.instance_id,r.workspace_id,r.issue_id,r.task_id,r.agent_id,r.purpose,$2 FROM decision_agent_reliance r WHERE r.instance_id=$1
        ON CONFLICT(reliance_id) DO NOTHING RETURNING reliance_id::text,task_id::text,agent_id::text,purpose`, instance, reason)
	if err != nil {
		return err
	}
	reliances := []agentDecisionRelianceRef{}
	relianceIDs := []string{}
	for rows.Next() {
		var r agentDecisionRelianceRef
		if err := rows.Scan(&r.RelianceID, &r.TaskID, &r.AgentID, &r.Purpose); err != nil {
			rows.Close()
			return err
		}
		reliances = append(reliances, r)
		relianceIDs = append(relianceIDs, r.RelianceID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var evidence []byte
	var candidate, gate string
	if err := tx.QueryRow(ctx, `SELECT evidence,candidate_sha256,waived_gate FROM decision_agent_scope WHERE instance_id=$1`, instance).Scan(&evidence, &candidate, &gate); err != nil {
		return err
	}
	details, err := json.Marshal(map[string]any{"decision_instance": instance, "candidate_sha256": candidate, "waived_gate": gate,
		"reason": reason, "reliance_ids": relianceIDs, "reliances": reliances, "evidence": json.RawMessage(evidence)})
	if err != nil {
		return err
	}
	var list strings.Builder
	for _, r := range reliances {
		fmt.Fprintf(&list, "\n- reliance %s: agent %s, task %s, for %s", r.RelianceID, r.AgentID, r.TaskID, r.Purpose)
	}
	if list.Len() == 0 {
		list.WriteString("\n- no reliance was recorded; confirm nothing else leaned on it")
	}
	_, err = tx.Exec(ctx, `INSERT INTO inbox_item(workspace_id,recipient_type,recipient_id,type,severity,issue_id,title,body,actor_type,actor_id,details)
        VALUES($1,$2,$3::uuid,'agent_decision_undo','action_required',$4,$5,$6,'system',NULL,$7)`,
		workspaceID, owner.kind, owner.id, issueID, "Undo what relied on an agent decision",
		fmt.Sprintf("Decision %s was revoked: %s\nYou own the undo. Reverify the evidence in this item, undo the work below and report it on this issue.%s", instance, reason, list.String()), details)
	return err
}

// A proposal may replace an earlier one by the same author for the same gate.
// The earlier instance is revoked in the same transaction as the new proposal.
func supersedeAgentDecision(ctx context.Context, tx pgx.Tx, params db.CreateCommentParams, successor string, proposal agentDecisionProposal) error {
	if proposal.Supersedes == "" {
		return nil
	}
	old, err := uuid.Parse(proposal.Supersedes)
	if err != nil || old == uuid.Nil || old.String() != proposal.Supersedes || old.String() == successor {
		return agentDecisionRefusal("supersedes must name another canonical instance")
	}
	if err := lockAgentDecision(ctx, tx, proposal.Supersedes); err != nil {
		return err
	}
	var found bool
	err = tx.QueryRow(ctx, `SELECT true FROM decision_agent_scope s
        JOIN decision_request r ON r.id=s.instance_id AND r.authority='agents' AND r.workspace_id=s.workspace_id AND r.issue_id=s.issue_id
        WHERE s.instance_id=$1 AND s.workspace_id=$2 AND s.issue_id=$3 AND s.waived_gate=$4 AND s.work_author_id=$5`,
		proposal.Supersedes, params.WorkspaceID, params.IssueID, proposal.WaivedGate, params.AuthorID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentDecisionRefusal("supersedes must name your own instance for the same gate on this issue")
	}
	if err != nil {
		return err
	}
	return revokeAgentDecision(ctx, tx, params.WorkspaceID, params.IssueID, proposal.Supersedes, params.AuthorID, agentDecisionUndoOwner{"agent", uuidToString(params.AuthorID)}, "superseded by decision:"+successor)
}
