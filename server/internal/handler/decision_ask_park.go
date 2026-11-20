package handler

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Workspace setting decision_ask_park (CHE-6460) decides when a decision
// request parks its issue. "always" (also an absent key) parks on every
// request. "on_wait" parks only when the asker sets wait, or when the issue is
// already blocked: an asker that does not wait keeps its status, assignee and
// Readiness and goes on working. Set it back to "always" to undo.
const (
	decisionAskParkKey      = "decision_ask_park"
	decisionAskParkAlways   = "always"
	decisionAskParkOnWait   = "on_wait"
	decisionAskParkErrorMsg = "decision_ask_park must be always or on_wait"
)

// validateDecisionAskParkSetting checks the decision_ask_park key of a
// workspace settings object. An absent key is valid; null is not.
func validateDecisionAskParkSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config[decisionAskParkKey]
	if !present {
		return nil
	}
	if s, _ := v.(string); s != decisionAskParkAlways && s != decisionAskParkOnWait {
		return fmt.Errorf("%s", decisionAskParkErrorMsg)
	}
	return nil
}

// decisionAskParkOnWaitSet reads the workspace setting. A malformed stored
// value reads as "always", the default: a bad setting must not stop a request.
func decisionAskParkOnWaitSet(settings []byte) bool {
	var stored struct {
		Mode string `json:"decision_ask_park"`
	}
	_ = json.Unmarshal(settings, &stored)
	return stored.Mode == decisionAskParkOnWait
}

// parkForDecisionRequest parks the issue unless decision_ask_park=on_wait and
// the asker neither set wait nor works on a blocked issue. It runs in the
// request transaction, at both park call sites.
func parkForDecisionRequest(ctx context.Context, queries *db.Queries, issue db.Issue, wait bool) (db.Issue, *db.ActivityLog, error) {
	if !wait && issue.Status != issuestatus.Blocked {
		workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
		if err != nil {
			return issue, nil, err
		}
		if decisionAskParkOnWaitSet(workspace.Settings) {
			return issue, nil, nil
		}
	}
	return parkIssueForDecision(ctx, queries, issue)
}
