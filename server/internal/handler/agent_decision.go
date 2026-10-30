package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Changes to this delegation/allowlist require a human instruction change.
const agentDecisionAllowlistVersion = 1
const agentDecisionDelegation = "edc6e2c4-c682-4219-a864-7d11728f6f83"

func agentDecisionGateAllowed(gate string) bool {
	switch gate {
	case "screenshot_evidence", "design_lock_label", "unrelated_base_failing_check":
		return true
	default:
		return false
	}
}

type agentDecisionEvidence struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}
type agentDecisionDispute struct {
	ReviewerCommentID string `json:"reviewer_comment_id"`
	ReviewerTaskID    string `json:"reviewer_task_id"`
	ReviewerVerdict   string `json:"reviewer_verdict"`
	AuthorCommentID   string `json:"author_comment_id"`
	AuthorTaskID      string `json:"author_task_id"`
	AuthorRebuttal    string `json:"author_rebuttal"`
	Point             string `json:"point"`
	AdjudicatorID     string `json:"adjudicator_agent_id"`
}
type agentDecisionProposal struct {
	WorkAuthorID          string                  `json:"work_author_id"`
	WaivedGate            string                  `json:"waived_gate"`
	Route                 string                  `json:"route"`
	Evidence              []agentDecisionEvidence `json:"evidence"`
	Signers               []string                `json:"signers,omitempty"`
	Dispute               *agentDecisionDispute   `json:"adjudication,omitempty"`
	Supersedes            string                  `json:"supersedes,omitempty"`
	BaseSHA               string                  `json:"base_sha,omitempty"`
	BaseFailedChecks      []string                `json:"base_failed_checks,omitempty"`
	CandidateFailedChecks []string                `json:"candidate_failed_checks,omitempty"`
}
type agentDecisionIdentity struct {
	AgentID, TaskID, Provider, Model, Lab string
}
type agentDecisionScope struct {
	Proposal   agentDecisionProposal
	Author     agentDecisionIdentity
	Signers    []agentDecisionIdentity
	Disputants []agentDecisionIdentity
}
type agentDecisionLine struct{ kind, instance, digest, commitment, verdict string }
type agentDecisionAnnotation struct {
	line           agentDecisionLine
	event, verdict string
	// Set for a veto: who vetoes and why. Revocation runs with the snapshot.
	vetoReason string
}
type agentDecisionEvent struct{ agent, kind, verdict, commitment, provider, model string }

func agentDecisionRefusal(message string) error {
	return decisionIntentRefusal{http.StatusBadRequest, "agent decision: " + message}
}

func agentDecisionDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// One protocol action per comment. A decision subject never enters the older
// governed-field tally, and malformed decision actions fail closed.
func parseAgentDecisionLine(body string) (*agentDecisionLine, error) {
	var protocol []string
	hasDecision := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		for _, prefix := range []string{"Proposal:", "Commit:", "Vote:", "Retract:", "Veto:"} {
			if strings.HasPrefix(line, prefix) {
				protocol = append(protocol, line)
				hasDecision = hasDecision || strings.Contains(line, "decision:")
				break
			}
		}
	}
	if !hasDecision {
		return nil, nil
	}
	if len(protocol) != 1 {
		return nil, agentDecisionRefusal("exactly one protocol line is required")
	}
	fields := strings.Fields(protocol[0])
	line := &agentDecisionLine{}
	switch fields[0] {
	case "Proposal:":
		if len(fields) != 3 {
			return nil, agentDecisionRefusal("invalid Proposal line")
		}
		line.kind = "proposal"
		line.instance = fields[1]
		line.digest = fields[2]
	case "Commit:":
		if len(fields) != 4 || !sha256HexPattern.MatchString(fields[3]) {
			return nil, agentDecisionRefusal("invalid Commit line")
		}
		line.kind = "commit"
		line.instance = fields[1]
		line.digest = fields[2]
		line.commitment = fields[3]
	case "Vote:":
		if len(fields) != 4 || (fields[1] != "PASS" && fields[1] != "BLOCK") {
			return nil, agentDecisionRefusal("invalid Vote line")
		}
		line.kind = "vote"
		line.verdict = fields[1]
		line.instance = fields[2]
		line.digest = fields[3]
	case "Retract:":
		if len(fields) != 2 {
			return nil, agentDecisionRefusal("invalid Retract line")
		}
		line.kind = "retract"
		line.instance = fields[1]
	case "Veto:":
		if len(fields) != 3 {
			return nil, agentDecisionRefusal("invalid Veto line")
		}
		line.kind = "veto"
		line.instance = fields[1]
		line.digest = fields[2]
	default:
		return nil, agentDecisionRefusal("unknown protocol line")
	}
	if !strings.HasPrefix(line.instance, "decision:") {
		return nil, agentDecisionRefusal("missing decision subject")
	}
	line.instance = strings.TrimPrefix(line.instance, "decision:")
	id, err := uuid.Parse(line.instance)
	if err != nil || id == uuid.Nil || id.String() != line.instance {
		return nil, agentDecisionRefusal("instance must be a canonical UUID")
	}
	if line.kind != "retract" && !sha256HexPattern.MatchString(line.digest) {
		return nil, agentDecisionRefusal("candidate must be a SHA256")
	}
	return line, nil
}

// Preserve LF-normalized bytes, including the final newline, without fences.
// Require a single exact-info block so no renderer chooses a different opinion.
var agentDecisionFencePattern = regexp.MustCompile("^[ \t]{0,3}(`{3,}|~{3,})[ \t]*(opinion|decision)[ \t]*$")

func agentDecisionBlock(body, info string) (string, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.SplitAfter(body, "\n")
	var out strings.Builder
	found, inside, closed := false, false, false
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		if fence := agentDecisionFencePattern.FindStringSubmatch(line); len(fence) == 3 && fence[2] == info && line != "```"+info {
			return "", agentDecisionRefusal("use an exact triple-backtick " + info + " fence")
		}
		if line == "```"+info {
			if found {
				return "", agentDecisionRefusal("duplicate " + info + " block")
			}
			found = true
			inside = true
			continue
		}
		if inside && line == "```" {
			inside = false
			closed = true
			continue
		}
		if inside {
			out.WriteString(raw)
		}
	}
	if !found || inside || !closed || out.Len() == 0 {
		return "", agentDecisionRefusal("one closed " + info + " block is required")
	}
	return out.String(), nil
}

func decodeAgentDecisionJSON(raw string, out any) error {
	if err := rejectDuplicateJSONKeys([]byte(raw)); err != nil {
		return agentDecisionRefusal("duplicate or invalid JSON field")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return agentDecisionRefusal("unknown or invalid proposal field")
	}
	return nil
}

func validateAgentDecisionProposal(p agentDecisionProposal, author string) error {
	if !agentDecisionGateAllowed(p.WaivedGate) {
		return agentDecisionExcluded("gate is not on allowlist version 1", "excluded category proposed: gate "+p.WaivedGate)
	}
	if p.WorkAuthorID != author {
		return agentDecisionRefusal("work author must be the proposer")
	}
	if len(p.Evidence) == 0 {
		return agentDecisionRefusal("evidence is required")
	}
	seen := map[string]bool{}
	for _, ref := range p.Evidence {
		u, err := url.Parse(ref.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || !sha256HexPattern.MatchString(ref.SHA256) || seen[ref.URL] {
			return agentDecisionRefusal("each evidence item needs a unique URL and SHA256")
		}
		seen[ref.URL] = true
	}
	if p.WaivedGate == "unrelated_base_failing_check" {
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p.BaseSHA) || len(p.BaseFailedChecks) == 0 || !slices.Equal(p.BaseFailedChecks, p.CandidateFailedChecks) {
			return agentDecisionRefusal("name the same failing checks on a pinned base and candidate")
		}
		checks := map[string]bool{}
		for _, check := range p.BaseFailedChecks {
			if strings.TrimSpace(check) == "" || checks[check] {
				return agentDecisionRefusal("failing check names must be distinct")
			}
			checks[check] = true
		}
	}
	switch p.Route {
	case "cosign":
		if len(p.Signers) != 2 || p.Signers[0] == p.Signers[1] || slices.Contains(p.Signers, author) || p.Dispute != nil {
			return agentDecisionRefusal("cosign needs two distinct named non-author signers")
		}
	case "adjudicate":
		if p.Dispute == nil || len(p.Signers) != 0 || p.Dispute.Point == "" || p.Dispute.ReviewerVerdict == "" || p.Dispute.AuthorRebuttal == "" {
			return agentDecisionRefusal("adjudicate needs a bound author/reviewer dispute")
		}
	default:
		return agentDecisionRefusal("unknown route")
	}
	return nil
}

func agentDecisionTaskIdentity(ctx context.Context, tx pgx.Tx, taskID, agentID, workspaceID, issueID pgtype.UUID) (agentDecisionIdentity, error) {
	var identity agentDecisionIdentity
	err := tx.QueryRow(ctx, `SELECT a.id::text,t.id::text,r.provider,COALESCE(t.claimed_model,'')
        FROM agent_task_queue t JOIN agent a ON a.id=t.agent_id AND a.workspace_id=$3 AND a.archived_at IS NULL
        JOIN agent_runtime r ON r.id=t.runtime_id AND r.workspace_id=$3
        WHERE t.id=$1 AND t.agent_id=$2 AND t.issue_id=$4 AND t.chat_session_id IS NULL AND t.status='running'
        FOR SHARE OF t,a,r`, taskID, agentID, workspaceID, issueID).Scan(&identity.AgentID, &identity.TaskID, &identity.Provider, &identity.Model)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, agentDecisionRefusal("a live same-issue task is required")
	}
	if err != nil {
		return identity, err
	}
	identity.Lab = voteLab(identity.Provider, identity.Model)
	if identity.Lab == "" {
		return identity, agentDecisionRefusal("task lab is unknown")
	}
	return identity, nil
}

func agentDecisionNamedIdentity(ctx context.Context, tx pgx.Tx, id string, workspaceID, issueID pgtype.UUID) (agentDecisionIdentity, error) {
	var identity agentDecisionIdentity
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return identity, agentDecisionRefusal("signer must be a canonical agent UUID")
	}
	err = tx.QueryRow(ctx, `SELECT a.id::text,COALESCE(live.provider,r.provider),COALESCE(CASE WHEN live.provider IS NULL THEN a.model ELSE live.claimed_model END,'') FROM agent a
        JOIN agent_runtime r ON r.id=a.runtime_id AND r.workspace_id=a.workspace_id
        LEFT JOIN LATERAL (SELECT rt.provider,t.claimed_model FROM agent_task_queue t
            JOIN agent_runtime rt ON rt.id=t.runtime_id AND rt.workspace_id=a.workspace_id
            WHERE t.agent_id=a.id AND t.issue_id=$3 AND t.status='running'
            ORDER BY t.created_at DESC,t.id DESC LIMIT 1) live ON true
        WHERE a.id=$1 AND a.workspace_id=$2 AND a.archived_at IS NULL FOR SHARE OF a,r`, id, workspaceID, issueID).Scan(&identity.AgentID, &identity.Provider, &identity.Model)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, agentDecisionRefusal("named signer is unavailable")
	}
	if err != nil {
		return identity, err
	}
	identity.Lab = voteLab(identity.Provider, identity.Model)
	if identity.Lab == "" {
		return identity, agentDecisionRefusal("named signer's lab is unknown")
	}
	return identity, nil
}

func agentDecisionDisputant(ctx context.Context, tx pgx.Tx, commentID, taskID, body string, workspaceID, issueID pgtype.UUID) (agentDecisionIdentity, error) {
	var identity agentDecisionIdentity
	cid, err := uuid.Parse(commentID)
	if err != nil {
		return identity, agentDecisionRefusal("invalid dispute comment")
	}
	tid, err := uuid.Parse(taskID)
	if err != nil {
		return identity, agentDecisionRefusal("invalid dispute task")
	}
	// Only the original task-authored, unedited source may attest the dispute.
	err = tx.QueryRow(ctx, `SELECT c.author_id::text,t.id::text,r.provider,COALESCE(t.claimed_model,'')
        FROM comment c JOIN agent_task_queue t ON t.id=c.source_task_id AND t.agent_id=c.author_id AND t.issue_id=c.issue_id
        JOIN agent_runtime r ON r.id=t.runtime_id AND r.workspace_id=c.workspace_id
        WHERE c.id=$1 AND c.source_task_id=$2 AND c.workspace_id=$3 AND c.issue_id=$4
          AND c.author_type='agent' AND c.deleted_at IS NULL AND c.revision=1 AND c.content=$5 FOR SHARE OF c,t,r`, cid.String(), tid.String(), workspaceID, issueID, body).Scan(&identity.AgentID, &identity.TaskID, &identity.Provider, &identity.Model)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, agentDecisionRefusal("dispute source does not match the original signed body")
	}
	if err != nil {
		return identity, err
	}
	identity.Lab = voteLab(identity.Provider, identity.Model)
	if identity.Lab == "" {
		return identity, agentDecisionRefusal("disputant lab is unknown")
	}
	return identity, nil
}

func createAgentDecisionInstance(ctx context.Context, tx pgx.Tx, params db.CreateCommentParams, line agentDecisionLine, author agentDecisionIdentity) error {
	raw, err := agentDecisionBlock(params.Content, "decision")
	if err != nil {
		return err
	}
	var proposal agentDecisionProposal
	if err := decodeAgentDecisionJSON(raw, &proposal); err != nil {
		return err
	}
	if err := validateAgentDecisionProposal(proposal, author.AgentID); err != nil {
		return err
	}
	scope := agentDecisionScope{Proposal: proposal, Author: author}
	names := proposal.Signers
	if proposal.Route == "adjudicate" {
		names = []string{proposal.Dispute.AdjudicatorID}
	}
	for _, name := range names {
		signer, err := agentDecisionNamedIdentity(ctx, tx, name, params.WorkspaceID, params.IssueID)
		if err != nil {
			return err
		}
		if signer.AgentID == author.AgentID {
			return agentDecisionRefusal("author cannot sign")
		}
		scope.Signers = append(scope.Signers, signer)
	}
	if proposal.Route == "cosign" {
		if scope.Signers[0].Lab == scope.Signers[1].Lab || (scope.Signers[0].Lab == author.Lab && scope.Signers[1].Lab == author.Lab) {
			return agentDecisionRefusal("signer labs must differ and include a non-author lab")
		}
	} else {
		dispute := proposal.Dispute
		reviewer, err := agentDecisionDisputant(ctx, tx, dispute.ReviewerCommentID, dispute.ReviewerTaskID, dispute.ReviewerVerdict, params.WorkspaceID, params.IssueID)
		if err != nil {
			return err
		}
		rebuttal, err := agentDecisionDisputant(ctx, tx, dispute.AuthorCommentID, dispute.AuthorTaskID, dispute.AuthorRebuttal, params.WorkspaceID, params.IssueID)
		if err != nil {
			return err
		}
		// A co-signer split is never an adjudication input. Both source bodies
		// explicitly bind the same gate and disputed point; excluded BLOCKs fail closed.
		if rebuttal.AgentID != author.AgentID || reviewer.AgentID == author.AgentID || reviewer.Lab == author.Lab ||
			scope.Signers[0].AgentID == reviewer.AgentID || scope.Signers[0].Lab == author.Lab || scope.Signers[0].Lab == reviewer.Lab {
			return agentDecisionRefusal("adjudication needs a third lab and a genuine author/reviewer dispute")
		}
		for _, body := range []string{dispute.ReviewerVerdict, dispute.AuthorRebuttal} {
			if strings.Contains(body, "decision:") || !strings.Contains(body, "Disputed-point: "+dispute.Point+"\n") || !strings.Contains(body, "Gate: "+proposal.WaivedGate+"\n") {
				return agentDecisionRefusal("dispute must bind the gate and point, not a co-signer split")
			}
		}
		if !strings.HasPrefix(dispute.ReviewerVerdict, "Verdict: BLOCK\n") || !strings.Contains(dispute.ReviewerVerdict, "Block-category: reversible_gate\n") {
			return agentDecisionRefusal("reviewer BLOCK must be for this reversible gate")
		}
		for _, excluded := range []string{"security", "test-first", "test_first", "merge approval", "deploy", "spend", "publication", "irreversible", "product direction", "missing evidence", "independently protected"} {
			if strings.Contains(strings.ToLower(dispute.ReviewerVerdict), excluded) {
				return agentDecisionExcluded("excluded or missing-evidence BLOCK cannot be adjudicated", "excluded category in adjudication: "+excluded)
			}
		}
		scope.Disputants = []agentDecisionIdentity{rebuttal, reviewer}
	}
	encoded, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(proposal.Evidence)
	if err != nil {
		return err
	}
	// The instance key is single-use across every workspace and issue.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM decision_request WHERE id=$1)`, line.instance).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return agentDecisionRefusal("instance already exists")
	}
	_, err = tx.Exec(ctx, `INSERT INTO decision_request(id,workspace_id,issue_id,asking_agent_id,authority,ask,escalation_class,origin,source_comment_id,source_content_snapshot)
        VALUES($1,$2,$3,$4,'agents',$5,'reversible','asked',$6,$5)`, line.instance, params.WorkspaceID, params.IssueID, params.AuthorID, params.Content, params.ID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO decision_agent_scope(instance_id,workspace_id,issue_id,proposer_agent_id,proposer_task_id,proposer_provider,proposer_model,proposer_lab,
        work_author_id,candidate_sha256,waived_gate,evidence,route,scope,delegation_id,allowlist_version,proposal_body,proposal_sha256)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$4,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, line.instance, params.WorkspaceID, params.IssueID, params.AuthorID, params.SourceTaskID,
		author.Provider, author.Model, author.Lab, line.digest, proposal.WaivedGate, evidence, proposal.Route, encoded, agentDecisionDelegation, agentDecisionAllowlistVersion, params.Content, agentDecisionDigest(params.Content))
	var constraint *pgconn.PgError
	if errors.As(err, &constraint) && constraint.Code == "23505" && constraint.ConstraintName == "agent_decision_scope_candidate_gate_uidx" {
		return agentDecisionRefusal("candidate and gate already have an immutable instance; signers cannot be replaced")
	}
	if err != nil {
		return err
	}
	return supersedeAgentDecision(ctx, tx, params, line.instance, proposal)
}

func loadAgentDecisionEvents(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID, instance string) ([]agentDecisionEvent, error) {
	rows, err := tx.Query(ctx, `SELECT author_id::text,decision_event,COALESCE(decision_verdict,''),COALESCE(commitment_sha256,''),runtime_provider,model
        FROM governed_vote_record WHERE workspace_id=$1 AND decision_instance_id=$2 ORDER BY decision_sequence`, workspaceID, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []agentDecisionEvent
	for rows.Next() {
		var e agentDecisionEvent
		if err := rows.Scan(&e.agent, &e.kind, &e.verdict, &e.commitment, &e.provider, &e.model); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func validateAgentDecisionOpinion(body string, line agentDecisionLine, scope agentDecisionScope, proposalSHA, commitment string) (string, error) {
	opinion, err := agentDecisionBlock(body, "opinion")
	if err != nil {
		return "", err
	}
	if agentDecisionDigest(opinion) != commitment {
		return "", agentDecisionRefusal("opinion differs from commitment")
	}
	fields := map[string]string{}
	header := true
	required := map[string]string{"decision_instance": line.instance, "candidate_sha256": line.digest, "waived_gate": scope.Proposal.WaivedGate,
		"proposal_sha256": proposalSHA, "independence": "formed before reading any other signer's opinion"}
	for _, raw := range strings.Split(opinion, "\n") {
		if raw == "" {
			header = false
			continue
		}
		key, value, ok := strings.Cut(raw, ": ")
		if !ok {
			if header {
				return "", agentDecisionRefusal("invalid opinion header")
			}
			continue
		}
		_, bound := required[key]
		bound = bound || key == "signer_agent_id" || key == "evidence" || key == "verdict" || key == "nonce" || key == "upholds" || key == "disputed_point"
		if !header {
			if bound {
				return "", agentDecisionRefusal("bound field outside opinion header")
			}
			continue
		}
		if !bound {
			return "", agentDecisionRefusal("unknown opinion field")
		}
		if _, duplicate := fields[key]; duplicate {
			return "", agentDecisionRefusal("duplicate opinion field")
		}
		fields[key] = value
	}
	for key, want := range required {
		if fields[key] != want {
			return "", agentDecisionRefusal("opinion field mismatch: " + key)
		}
	}
	if fields["verdict"] != line.verdict {
		return "", agentDecisionRefusal("Vote verdict differs from committed verdict")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(fields["nonce"]) {
		return "", agentDecisionRefusal("nonce needs 32 random hex characters")
	}
	var evidence []agentDecisionEvidence
	if err := decodeAgentDecisionJSON(fields["evidence"], &evidence); err != nil {
		return "", err
	}
	if !slices.Equal(evidence, scope.Proposal.Evidence) {
		return "", agentDecisionRefusal("opinion evidence differs from proposal")
	}
	if scope.Proposal.Route == "adjudicate" {
		if fields["disputed_point"] != scope.Proposal.Dispute.Point || (fields["upholds"] != "author" && fields["upholds"] != "reviewer") ||
			(fields["upholds"] == "author") != (fields["verdict"] == "PASS") {
			return "", agentDecisionRefusal("adjudicator must resolve the bound point for one side")
		}
	} else if fields["upholds"] != "" || fields["disputed_point"] != "" {
		return "", agentDecisionRefusal("cosign cannot adjudicate")
	}
	return fields["signer_agent_id"], nil
}

// The caller owns the comment transaction. No decision state becomes effective
// here: C1 only records immutable proposals, commitments, reveals and retractions.
func prepareAgentDecision(ctx context.Context, tx pgx.Tx, params db.CreateCommentParams, taskToken bool) (*agentDecisionAnnotation, error) {
	line, err := parseAgentDecisionLine(params.Content)
	if err != nil || line == nil {
		return nil, err
	}
	// A workspace owner may veto without a task; every other action is a task's.
	ownerVeto := line.kind == "veto" && params.AuthorType == "member"
	if !ownerVeto && (!taskToken || params.AuthorType != "agent" || !params.SourceTaskID.Valid) {
		return nil, agentDecisionRefusal("an issue task token is required")
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agent-decision:' || $1,0))`, line.instance); err != nil {
		return nil, err
	}
	var identity agentDecisionIdentity
	if !ownerVeto {
		identity, err = agentDecisionTaskIdentity(ctx, tx, params.SourceTaskID, params.AuthorID, params.WorkspaceID, params.IssueID)
		if err != nil {
			return nil, err
		}
	}
	annotation := &agentDecisionAnnotation{line: *line, event: line.kind}
	if line.kind == "proposal" {
		return annotation, createAgentDecisionInstance(ctx, tx, params, *line, identity)
	}
	var encoded []byte
	var digest, proposalSHA string
	var version int
	var delegation string
	err = tx.QueryRow(ctx, `SELECT s.scope,s.candidate_sha256,s.proposal_sha256,s.allowlist_version,s.delegation_id::text FROM decision_agent_scope s
        JOIN decision_request r ON r.id=s.instance_id AND r.authority='agents' AND r.workspace_id=s.workspace_id AND r.issue_id=s.issue_id
        WHERE s.instance_id=$1 AND s.workspace_id=$2 AND s.issue_id=$3`, line.instance, params.WorkspaceID, params.IssueID).Scan(&encoded, &digest, &proposalSHA, &version, &delegation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, agentDecisionRefusal("instance is not on this issue")
	}
	if err != nil {
		return nil, err
	}
	var scope agentDecisionScope
	if err := json.Unmarshal(encoded, &scope); err != nil {
		return nil, err
	}
	if version != agentDecisionAllowlistVersion || delegation != agentDecisionDelegation || !agentDecisionGateAllowed(scope.Proposal.WaivedGate) {
		return nil, agentDecisionRefusal("delegation or allowlist no longer matches")
	}
	if line.kind != "retract" && line.digest != digest {
		return nil, agentDecisionRefusal("candidate digest does not match instance")
	}
	var settled, revoked bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(bool_or(true),false),COALESCE(bool_or(revoked_at IS NOT NULL),false) FROM decision_agent_settlement WHERE instance_id=$1`, line.instance).Scan(&settled, &revoked); err != nil {
		return nil, err
	}
	if revoked {
		return nil, agentDecisionRefusal("instance is revoked and can never become effective")
	}
	if line.kind == "veto" {
		return annotation, authorizeAgentDecisionVeto(ctx, tx, params, annotation, scope, identity.AgentID, ownerVeto)
	}
	if line.kind == "retract" && settled {
		return nil, agentDecisionRefusal("an effective record can only be vetoed")
	}
	signer := slices.IndexFunc(scope.Signers, func(s agentDecisionIdentity) bool { return s.AgentID == identity.AgentID })
	if signer < 0 || identity.AgentID == scope.Author.AgentID || scope.Signers[signer].Lab != identity.Lab {
		return nil, agentDecisionRefusal("task is not a named signer of the frozen lab")
	}
	events, err := loadAgentDecisionEvents(ctx, tx, params.WorkspaceID, line.instance)
	if err != nil {
		return nil, err
	}
	commits := map[string]string{}
	var mine []agentDecisionEvent
	for _, e := range events {
		if e.kind == "commit" {
			commits[e.agent] = e.commitment
		}
		if e.agent == identity.AgentID {
			mine = append(mine, e)
		}
	}
	has := func(kind string) bool {
		return slices.ContainsFunc(mine, func(e agentDecisionEvent) bool { return e.kind == kind })
	}
	if has("retract") || has("void") {
		return nil, agentDecisionRefusal("signer retracted or revealed early and cannot be replaced")
	}
	switch line.kind {
	case "commit":
		if has("commit") || has("vote") {
			return nil, agentDecisionRefusal("one initial commitment per signer")
		}
		if strings.TrimSpace(params.Content) != "Commit: decision:"+line.instance+" "+line.digest+" "+line.commitment {
			return nil, agentDecisionRefusal("commit must contain only the commitment line")
		}
	case "vote":
		if !has("commit") || has("vote") {
			return nil, agentDecisionRefusal("one reveal after a commitment is required")
		}
		signerID, err := validateAgentDecisionOpinion(params.Content, *line, scope, proposalSHA, commits[identity.AgentID])
		if err != nil {
			return nil, err
		}
		if signerID != identity.AgentID {
			return nil, agentDecisionRefusal("opinion signer differs from trusted task author")
		}
		if len(commits) != len(scope.Signers) {
			annotation.event = "void"
		} else {
			annotation.verdict = line.verdict
		}
	case "retract":
		if !has("commit") {
			return nil, agentDecisionRefusal("nothing to retract")
		}
	}
	return annotation, nil
}

// A refused reveal has no comment row. Its allocated comment ID identifies only
// this rejected attempt; the immutable body and casting task remain auditable.
func recordAgentDecisionVoid(ctx context.Context, tx pgx.Tx, params db.CreateCommentParams, annotation *agentDecisionAnnotation) error {
	_, err := tx.Exec(ctx, `INSERT INTO governed_vote_record
        (issue_id,comment_id,workspace_id,author_type,author_id,vote_lines,created_at,
         runtime_provider,model,full_body,source_task_id,decision_instance_id,decision_event)
        SELECT $1,$2,$3,'agent',$4,$5,clock_timestamp(),r.provider,COALESCE(t.claimed_model,''),$6,t.id,$7,'void'
        FROM agent_task_queue t JOIN agent_runtime r ON r.id=t.runtime_id
        WHERE t.id=$8 AND t.agent_id=$4 AND t.issue_id=$1 AND t.status='running'`,
		params.IssueID, params.ID, params.WorkspaceID, params.AuthorID,
		"Vote: "+annotation.line.verdict+" decision:"+annotation.line.instance+" "+annotation.line.digest,
		params.Content, annotation.line.instance, params.SourceTaskID)
	return err
}

func annotateAgentDecision(ctx context.Context, tx pgx.Tx, commentID pgtype.UUID, annotation *agentDecisionAnnotation) error {
	if annotation == nil {
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE governed_vote_record SET decision_instance_id=$1,decision_event=$2,
        decision_verdict=NULLIF($3,''),commitment_sha256=NULLIF($4,'') WHERE comment_id=$5 AND decision_instance_id IS NULL`,
		annotation.line.instance, annotation.event, annotation.verdict, annotation.line.commitment, commentID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("agent decision snapshot missing")
	}
	if annotation.event == "veto" {
		return revokeAgentDecisionForVeto(ctx, tx, commentID, annotation)
	}
	if annotation.event == "vote" {
		var workspaceID pgtype.UUID
		if err := tx.QueryRow(ctx, `SELECT workspace_id FROM comment WHERE id=$1`, commentID).Scan(&workspaceID); err != nil {
			return err
		}
		return settleAgentDecision(ctx, tx, workspaceID, annotation.line.instance)
	}
	return nil
}
