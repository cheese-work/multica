package handler

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"github.com/multica-ai/multica/server/internal/taskassert"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// doopAssertionKeyEnv is "<kid>:<base64 Ed25519 seed>". Unset means the claim
// carries no assertion and is otherwise unchanged (CHE-7064 D1.6).
const doopAssertionKeyEnv = "MULTICA_TASK_ASSERTION_SIGNING_KEY"

var (
	assertionWarnMu  sync.Mutex
	assertionWarnRaw string // the malformed value already logged
)

// doopTaskAssertion mints the signed task identity for a freshly claimed task,
// or returns "". It never fails a claim: a bad key or a failed mint only means
// no assertion. Nothing is stored.
func (h *Handler) doopTaskAssertion(ctx context.Context, task *db.AgentTaskQueue, rt db.AgentRuntime, resp *AgentTaskResponse) string {
	raw := os.Getenv(doopAssertionKeyEnv)
	if raw == "" {
		return ""
	}
	key, err := taskassert.ParseKey(raw)
	if err != nil {
		assertionWarnMu.Lock()
		first := assertionWarnRaw != raw
		assertionWarnRaw = raw
		assertionWarnMu.Unlock()
		if first { // log once per distinct value; never the key itself
			slog.Error("task assertion signing key is malformed; minting nothing", "env", doopAssertionKeyEnv, "error", err)
		}
		return ""
	}
	c := taskassert.Claims{
		Wid: resp.WorkspaceID,
		Tid: uuidToString(task.ID),
		Aid: uuidToString(task.AgentID),
		Prv: rt.Provider,
	}
	if resp.ClaimedModel != nil {
		c.Mdl = *resp.ClaimedModel
	}
	if task.IssueID.Valid { // chat tasks have no issue: iid and pid stay empty
		c.Iid = uuidToString(task.IssueID)
		issue, err := h.Queries.GetIssue(ctx, task.IssueID)
		if err != nil {
			slog.Warn("task assertion: load issue failed; minting nothing", "task_id", c.Tid, "error", err)
			return ""
		}
		if issue.ProjectID.Valid {
			c.Pid = uuidToString(issue.ProjectID)
		}
	}
	token, err := taskassert.Mint(c, key)
	if err != nil {
		slog.Warn("task assertion: mint failed", "task_id", c.Tid, "error", err)
		return ""
	}
	return token
}
