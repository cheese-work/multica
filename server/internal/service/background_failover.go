package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/bgagents"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ErrNoReachableBackgroundAgent: no candidate in the chain can take the job now.
// The caller runs the job's no-agent path.
var ErrNoReachableBackgroundAgent = errors.New("background task: no reachable agent in the chain")

// BackgroundNoAgentPath runs when a job's chain ends without an output: the
// primary and fallback were unreachable at enqueue, or the run that held the
// last slot failed.
type BackgroundNoAgentPath func(ctx context.Context, task db.AgentTaskQueue)

// EnqueueBackgroundChain queues the job on the first reachable candidate and
// remembers the next candidate as the fallback for one provider failure. A
// candidate that is unreachable now is skipped, not queued behind. The caller
// runs the no-agent path on ErrNoReachableBackgroundAgent.
func (s *TaskService) EnqueueBackgroundChain(ctx context.Context, in BackgroundTaskInput, chain []bgagents.Candidate) (db.AgentTaskQueue, error) {
	for i, c := range chain {
		if !s.backgroundAgentReachable(ctx, in.Job, in.WorkspaceID, c.AgentID) {
			logBackgroundFailover(in.Job, c.Slot, "unreachable")
			continue
		}
		in.AgentID, in.Slot, in.FallbackAgentID = util.MustParseUUID(c.AgentID), c.Slot, pgtype.UUID{}
		if c.Slot == bgagents.Primary && i+1 < len(chain) {
			in.FallbackAgentID = util.MustParseUUID(chain[i+1].AgentID)
		}
		return s.EnqueueBackgroundTask(ctx, in)
	}
	return db.AgentTaskQueue{}, ErrNoReachableBackgroundAgent
}

// backgroundAgentReachable mirrors what the claim needs: an eligible agent on a
// runtime that is online with a fresh heartbeat and, for an output_only job, a
// daemon that advertises the capability. A lookup error counts as unreachable.
func (s *TaskService) backgroundAgentReachable(ctx context.Context, job bgagents.Job, workspaceID pgtype.UUID, agentID string) bool {
	id, err := util.ParseUUID(agentID)
	if err != nil {
		return false
	}
	agent, err := s.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: workspaceID})
	if err != nil || !agent.RuntimeID.Valid {
		return false
	}
	rt, err := s.Queries.GetAgentRuntime(ctx, agent.RuntimeID)
	if err != nil || rt.Status != "online" || !bgagents.Eligible(job, agent, rt.Provider) {
		return false
	}
	seen := rt.LastSeenAt
	if !seen.Valid {
		seen = rt.UpdatedAt
	}
	if !seen.Valid || time.Since(seen.Time) > time.Duration(RuntimeClaimFreshnessSeconds)*time.Second {
		return false
	}
	if spec, ok := bgagents.SpecFor(job); ok && spec.Runner == bgagents.OutputOnly {
		var meta struct {
			Capabilities []string `json:"capabilities"`
		}
		_ = json.Unmarshal(rt.Metadata, &meta)
		return slices.Contains(meta.Capabilities, protocol.DaemonCapabilityBackgroundOutputOnlyV1)
	}
	return true
}

// failOverBackgroundTask is the failure rule for a background task whose run
// failed and will not be retried. A provider failure or a missed claim or run
// deadline on the primary slot runs the fallback once, if it is still reachable. Any other outcome ends the
// chain: a fallback failure, a non-provider failure, or no reachable fallback.
// The chain then runs the job's no-agent path. Best effort, like routing.
func (s *TaskService) failOverBackgroundTask(ctx context.Context, failed db.AgentTaskQueue, failureReason string) {
	var c BackgroundTaskContext
	if !strings.HasPrefix(failed.Purpose.String, "bg:") || json.Unmarshal(failed.Context, &c) != nil || c.Type != BackgroundTaskContextType {
		return
	}
	job, slot := bgagents.Job(c.Job), bgagents.SlotName(c.Slot)
	if (providerFailureReasons[failureReason] || backgroundDeadlineReasons[failureReason]) && slot == bgagents.Primary && c.FallbackAgentID != "" {
		workspaceID, _ := util.ParseUUID(c.WorkspaceID)
		if s.backgroundAgentReachable(ctx, job, workspaceID, c.FallbackAgentID) {
			next := BackgroundTaskInput{
				Job: job, WorkspaceID: workspaceID, AgentID: util.MustParseUUID(c.FallbackAgentID),
				Source: bgagents.Source(c.ChainSource), Slot: bgagents.Fallback, Input: c.Input,
				SourceChatSessionID: failed.SourceChatSessionID,
			}
			next.MemberID, _ = util.ParseUUID(c.MemberID)
			if _, err := s.EnqueueBackgroundTask(ctx, next); err == nil {
				logBackgroundFailover(job, slot, failureReason)
				return
			}
		} else {
			logBackgroundFailover(job, bgagents.Fallback, "unreachable")
		}
	}
	logBackgroundFailover(job, slot, failureReason)
	if run := s.BackgroundNoAgent[failed.Purpose.String]; run != nil {
		run(ctx, failed)
	}
}

// logBackgroundFailover carries the job, slot and reason only: no transcript
// text, input or output.
func logBackgroundFailover(job bgagents.Job, slot bgagents.SlotName, reason string) {
	slog.Warn("background failover", "job", string(job), "slot", string(slot), "reason", reason)
}

// failOverTask sends a failed task to the failover rule for its kind.
func (s *TaskService) failOverTask(ctx context.Context, failed db.AgentTaskQueue, failureReason string) {
	if strings.HasPrefix(failed.Purpose.String, "bg:") {
		s.failOverBackgroundTask(ctx, failed, failureReason)
		return
	}
	s.failOverRoutingTask(ctx, failed, failureReason)
}
