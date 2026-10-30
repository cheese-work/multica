package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decisionBindingTTL matches the 7 days an open request lives before it expires.
const decisionBindingTTL = 7 * 24 * time.Hour

var (
	sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	declaredHashLine = regexp.MustCompile(`(?m)^(before_sha256|after_sha256): ([0-9a-f]{64})$`)
)

// delegatedWriteGate reports whether agentID may make a delegated governed
// write to field, and whether that write needs a human decision.
type delegatedWriteGate func(ctx context.Context, qtx *db.Queries, workspaceID, agentID pgtype.UUID, field string) (requiresDecision, ok bool)

// verifyDecisionBinding refuses a governed write unless decisionID is a live,
// unexpired, unused approval by a workspace owner of the change from beforeSHA
// to afterSHA. It locks the request, then its live answer, as a withdrawal
// does, so no withdrawal commits before qtx's transaction ends. On success it
// records the write in decision_bound_write inside qtx's transaction: the
// primary key on decision_id makes the decision single-use.
func verifyDecisionBinding(ctx context.Context, qtx *db.Queries, allowed delegatedWriteGate, workspaceID, decisionID, agentID pgtype.UUID, field, beforeSHA, afterSHA string) error {
	refuse := func(status int, message string) error { return decisionIntentRefusal{status, message} }
	if !sha256HexPattern.MatchString(beforeSHA) || !sha256HexPattern.MatchString(afterSHA) {
		return refuse(http.StatusBadRequest, "invalid sha256")
	}
	if _, ok := allowed(ctx, qtx, workspaceID, agentID, field); !ok {
		return refuse(http.StatusForbidden, "agent is not allowed to write this field")
	}
	request, err := qtx.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: decisionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(http.StatusNotFound, "human decision request not found")
	}
	if err != nil {
		return err
	}
	if request.Authority != "human" {
		return refuse(http.StatusForbidden, "decision requires human authority")
	}
	answer, err := qtx.GetDecisionBindingAnswer(ctx, db.GetDecisionBindingAnswerParams{WorkspaceID: workspaceID, ID: decisionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(http.StatusForbidden, "decision has no live answer")
	}
	if err != nil {
		return err
	}
	switch {
	case answer.HumanRole != "owner":
		return refuse(http.StatusForbidden, "the decision's human is not a workspace owner")
	case !askDeclaresChange(request.Ask, field, beforeSHA, afterSHA):
		return refuse(http.StatusForbidden, "the ask does not name this field, before_sha256 and after_sha256")
	case !answerIsApproval(request.Options, answer.ContentSnapshot):
		return refuse(http.StatusForbidden, "the answer is not the approve option")
	case answer.EffectiveAt.Time.After(answer.Now.Time):
		return refuse(http.StatusConflict, "the answer is not yet effective")
	case answer.CreatedAt.Time.Add(decisionBindingTTL).Before(answer.Now.Time):
		return refuse(http.StatusForbidden, "decision expired")
	}
	inserted, err := qtx.InsertDecisionBoundWrite(ctx, db.InsertDecisionBoundWriteParams{
		DecisionID: decisionID, WorkspaceID: workspaceID, AgentID: agentID, Field: field, BeforeSha256: beforeSHA, AfterSha256: afterSHA,
	})
	if err != nil {
		return err
	}
	if inserted == 0 {
		return refuse(http.StatusConflict, "decision already used")
	}
	slog.Info("decision-bound write accepted", "workspace_id", uuidToString(workspaceID), "decision_id", uuidToString(decisionID),
		"agent_id", uuidToString(agentID), "field", field, "before_sha256", beforeSHA, "after_sha256", afterSHA)
	return nil
}

// askDeclaresChange is true when the ask is exactly the decisiontx.BoundWriteDecision
// text for this field and these hashes. The field line binds the approval to
// one field: another field holding the same bytes is not approved. A hash that
// appears elsewhere in the ask does not count.
func askDeclaresChange(ask, field, beforeSHA, afterSHA string) bool {
	if !strings.HasPrefix(ask, decisiontx.BoundWriteHeading(field)) {
		return false
	}
	declared := map[string]string{}
	for _, match := range declaredHashLine.FindAllStringSubmatch(ask, -1) {
		if _, repeated := declared[match[1]]; repeated {
			return false
		}
		declared[match[1]] = match[2]
	}
	return declared["before_sha256"] == beforeSHA && declared["after_sha256"] == afterSHA
}

// answerIsApproval is true when the card offered the approve option and the
// answer is exactly that label: a note typed with it makes the approval
// conditional, so it does not count.
func answerIsApproval(options []byte, answer string) bool {
	var offered []decisiontx.AskOption
	if err := json.Unmarshal(options, &offered); err != nil {
		return false
	}
	return strings.TrimSpace(answer) == decisiontx.ApproveWriteLabel &&
		slices.ContainsFunc(offered, func(option decisiontx.AskOption) bool { return option.Label == decisiontx.ApproveWriteLabel })
}
