package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// C2 must not create an effective record in a live workspace. There is no
// environment or request override. Lifecycle and pilot admission land later.
const agentDecisionSettlementCreationEnabled = false

func lockAgentDecision(ctx context.Context, tx pgx.Tx, instance string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agent-decision:' || $1,0))`, instance)
	return err
}

// Called after the immutable vote annotation, within the comment transaction.
// Tests exercise this dormant engine directly; the production call is disabled.
func settleAgentDecision(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, instance string) error {
	if err := lockAgentDecision(ctx, tx, instance); err != nil {
		return err
	}
	var encoded []byte
	var version int
	var delegation, gate, route string
	var issueID pgtype.UUID
	err := tx.QueryRow(ctx, `SELECT s.scope,s.allowlist_version,s.delegation_id::text,s.waived_gate,s.route,s.issue_id
 FROM decision_agent_scope s JOIN decision_request r ON r.id=s.instance_id AND r.workspace_id=s.workspace_id AND r.issue_id=s.issue_id AND r.authority='agents'
 WHERE s.workspace_id=$1 AND s.instance_id=$2`, workspaceID, instance).Scan(&encoded, &version, &delegation, &gate, &route, &issueID)
	if err != nil {
		return err
	}
	if version != agentDecisionAllowlistVersion || delegation != agentDecisionDelegation || !agentDecisionGateAllowed(gate) || route != "cosign" {
		return nil
	}
	var scope agentDecisionScope
	if err := json.Unmarshal(encoded, &scope); err != nil {
		return err
	}
	if len(scope.Signers) != 2 || scope.Signers[0].AgentID == scope.Signers[1].AgentID || scope.Signers[0].Lab == "" || scope.Signers[1].Lab == "" || scope.Signers[0].Lab == scope.Signers[1].Lab {
		return nil
	}
	events, err := loadAgentDecisionEvents(ctx, tx, workspaceID, instance)
	if err != nil {
		return err
	}
	for _, signer := range scope.Signers {
		if signer.AgentID == scope.Author.AgentID {
			return nil
		}
		commits, votes := 0, 0
		for _, e := range events {
			if e.agent != signer.AgentID {
				continue
			}
			if e.kind == "void" || e.kind == "retract" {
				return nil
			}
			if e.kind == "commit" {
				commits++
			}
			if e.kind == "vote" {
				if e.verdict != "PASS" || voteLab(e.provider, e.model) != signer.Lab {
					return nil
				}
				votes++
			}
		}
		if commits != 1 || votes != 1 {
			return nil
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO decision_agent_settlement(instance_id,workspace_id,issue_id) VALUES($1,$2,$3) ON CONFLICT(instance_id) DO NOTHING`, instance, workspaceID, issueID)
	return err
}

type agentDecisionBinding struct {
	Gate            string `json:"gate"`
	CandidateSHA256 string `json:"candidate_sha256"`
	Purpose         string `json:"purpose"`
}

func validAgentDecisionBinding(b agentDecisionBinding) bool {
	return agentDecisionGateAllowed(b.Gate) && sha256HexPattern.MatchString(b.CandidateSHA256)
}

// Display only. Even an effective record never returns authorization here.
func (h *Handler) LookupAgentDecision(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	b := agentDecisionBinding{Gate: r.URL.Query().Get("gate"), CandidateSHA256: r.URL.Query().Get("candidate_sha256")}
	if !validAgentDecisionBinding(b) {
		writeError(w, http.StatusBadRequest, "invalid agent decision binding")
		return
	}
	var instance, state string
	err := h.DB.QueryRow(r.Context(), `SELECT s.instance_id::text,CASE WHEN e.revoked_at IS NOT NULL THEN 'revoked' WHEN e.effective_at<=clock_timestamp() THEN 'effective' ELSE 'proposed' END
 FROM decision_agent_scope s JOIN decision_request d ON d.id=s.instance_id AND d.workspace_id=s.workspace_id AND d.issue_id=s.issue_id AND d.authority='agents'
 LEFT JOIN decision_agent_settlement e ON e.instance_id=s.instance_id AND e.workspace_id=s.workspace_id AND e.issue_id=s.issue_id
 WHERE s.workspace_id=$1 AND s.issue_id=$2 AND s.waived_gate=$3 AND s.candidate_sha256=$4`, issue.WorkspaceID, issue.ID, b.Gate, b.CandidateSHA256).Scan(&instance, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		state = "not decided"
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read agent decision")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instance_id": instance, "state": state, "decided": false, "authorization": false})
}

func (h *Handler) RelyAgentDecision(w http.ResponseWriter, r *http.Request) {
	closed := map[string]any{"decided": false, "state": "not decided"}
	if r.Header.Get("X-Actor-Source") != "task_token" {
		writeJSON(w, http.StatusForbidden, closed)
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var b agentDecisionBinding
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&b); err != nil || !validAgentDecisionBinding(b) || strings.TrimSpace(b.Purpose) == "" || len(b.Purpose) > 4096 {
		writeJSON(w, http.StatusBadRequest, closed)
		return
	}
	instance, reliance, err := h.relyAgentDecision(r, issue, b)
	if err != nil {
		writeJSON(w, http.StatusOK, closed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decided": true, "state": "decided", "instance_id": instance, "reliance_id": reliance})
}

// The instance is immutable. Locate it, acquire the veto lock, then validate
// and register reliance in the same transaction. Only commit can authorize.
func (h *Handler) relyAgentDecision(r *http.Request, issue db.Issue, b agentDecisionBinding) (string, string, error) {
	ctx := r.Context()
	task, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return "", "", err
	}
	agent, err := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	if err != nil {
		return "", "", err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var instance string
	err = tx.QueryRow(ctx, `SELECT instance_id::text FROM decision_agent_scope WHERE workspace_id=$1 AND issue_id=$2 AND waived_gate=$3 AND candidate_sha256=$4`, issue.WorkspaceID, issue.ID, b.Gate, b.CandidateSHA256).Scan(&instance)
	if err != nil {
		return "", "", err
	}
	if err := lockAgentDecision(ctx, tx, instance); err != nil {
		return "", "", err
	}
	var callingTask pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT t.id FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$3 AND a.archived_at IS NULL
 WHERE t.id=$1 AND t.agent_id=$2 AND t.status='running' AND t.issue_id=$4 AND t.chat_session_id IS NULL FOR SHARE OF t,a`, task, agent, issue.WorkspaceID, issue.ID).Scan(&callingTask)
	if err != nil {
		return "", "", err
	}
	var gate string
	var effective time.Time
	err = tx.QueryRow(ctx, `SELECT s.waived_gate,e.effective_at FROM decision_agent_scope s
 JOIN decision_request d ON d.id=s.instance_id AND d.workspace_id=s.workspace_id AND d.issue_id=s.issue_id AND d.authority='agents'
 JOIN decision_agent_settlement e ON e.instance_id=s.instance_id AND e.workspace_id=s.workspace_id AND e.issue_id=s.issue_id
 WHERE s.instance_id=$1 AND s.workspace_id=$2 AND s.issue_id=$3 AND s.candidate_sha256=$4 AND s.waived_gate=$5
 AND s.allowlist_version=$6 AND s.delegation_id=$7 AND e.revoked_at IS NULL AND e.effective_at<=clock_timestamp()`, instance, issue.WorkspaceID, issue.ID, b.CandidateSHA256, b.Gate, agentDecisionAllowlistVersion, agentDecisionDelegation).Scan(&gate, &effective)
	if err != nil {
		return "", "", err
	}
	if !agentDecisionGateAllowed(gate) {
		return "", "", agentDecisionRefusal("gate is no longer allowed")
	}
	var reliance string
	err = tx.QueryRow(ctx, `INSERT INTO decision_agent_reliance(instance_id,workspace_id,issue_id,task_id,agent_id,purpose) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, instance, issue.WorkspaceID, issue.ID, task, agent, b.Purpose).Scan(&reliance)
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return instance, reliance, nil
}
