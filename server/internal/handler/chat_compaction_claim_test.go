package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// After an in-place compaction (compaction_epoch > 0, session_id NULL) the next
// claim must not resume a pre-boundary provider session. It starts fresh and is
// seeded with the compaction summary plus the messages after the boundary.

const compactionSummaryText = "SUMMARY-OF-EARLIER-WORK"

// compactedDM builds a DM at epoch 1 with two pre-boundary messages, the
// compaction summary, two post-boundary messages, and a queued current input.
// A finished epoch-0 task holds "S_old" so a leaky fallback has something to
// resume. It returns the ids a claim needs.
func compactedDM(t *testing.T, provider string) (agentID, chatID, runtimeID, daemonID string) {
	t.Helper()
	ctx := context.Background()
	agentID, runtimeID, daemonID = createRuntimeGuardAgent(t, ctx)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = $2 WHERE id = $1`, runtimeID, provider)
	chatID = dbfx.ChatSession(t, agentID, testutil.Cols{
		"title":                 "compacted",
		"compaction_epoch":      1,
		"explicitly_created_at": testutil.Raw("now()"),
	})
	msg := func(role, kind, content, ago string) {
		cols := testutil.Cols{
			"chat_session_id": chatID, "role": role, "content": content,
			"created_at": testutil.Raw("now() - interval '" + ago + "'"),
		}
		if kind != "" {
			cols["message_kind"] = kind
		}
		dbfx.Insert(t, "chat_message", cols)
	}
	msg("user", "", "PRE-BOUNDARY-QUESTION", "10 minutes")
	msg("assistant", "", "PRE-BOUNDARY-ANSWER", "9 minutes")
	msg("assistant", "compaction_summary", compactionSummaryText, "5 minutes")
	msg("user", "", "POST-ONE", "4 minutes")
	msg("assistant", "", "POST-TWO", "3 minutes")
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "chat_session_id": chatID, "status": "completed",
		"started_at": testutil.Raw("now() - interval '10 minutes'"), "completed_at": testutil.Raw("now() - interval '9 minutes'"),
		"chat_compaction_epoch": 0, "session_id": "S_old", "work_dir": "/tmp/old-workdir",
	})
	sendDirectChat(t, ctx, agentID, chatID, "CURRENT-INPUT")
	return
}

// C1: a session started in the current epoch is the direct pointer; resume it
// and add no seed.
func TestCompactionClaim_CurrentEpochPointerResumesWithoutSeed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, chatID, runtimeID, daemonID := compactedDM(t, "claude")
	dbfx.Exec(t, `UPDATE chat_session SET session_id = 'S_new', runtime_id = $2 WHERE id = $1`, chatID, runtimeID)
	got := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if got.PriorSessionID != "S_new" || got.ChatMessage != "CURRENT-INPUT" {
		t.Fatalf("prior=%q message=%q, want S_new resumed with the bare input", got.PriorSessionID, got.ChatMessage)
	}
}

// C2: empty pointer and only an older-epoch session on record: start fresh with
// a seed.
func TestCompactionClaim_OlderEpochSessionIsNotResumed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, _, runtimeID, daemonID := compactedDM(t, "claude")
	got := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if got.PriorSessionID != "" {
		t.Fatalf("prior session = %q, want none (S_old predates the boundary)", got.PriorSessionID)
	}
	if !strings.Contains(got.ChatMessage, compactionSummaryText) {
		t.Fatalf("no seed in %q", got.ChatMessage)
	}
}

// C3: a follow-up that began before the boundary landed but was dispatched in
// the new epoch holds a current-epoch session; the fallback resumes it.
func TestCompactionClaim_CurrentEpochFallbackSessionResumes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, chatID, runtimeID, daemonID := compactedDM(t, "claude")
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "chat_session_id": chatID, "status": "completed",
		"started_at": testutil.Raw("now() - interval '2 minutes'"), "completed_at": testutil.Raw("now() - interval '1 minute'"),
		"chat_compaction_epoch": 1, "session_id": "S_q", "work_dir": "/tmp/new-workdir",
	})
	got := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if got.PriorSessionID != "S_q" || got.ChatMessage != "CURRENT-INPUT" {
		t.Fatalf("prior=%q message=%q, want S_q resumed with no seed", got.PriorSessionID, got.ChatMessage)
	}
}

// C4/C5: the seed is the summary plus post-boundary messages plus the current
// input once, with nothing from before the boundary; same for every provider.
func TestCompactionClaim_SeedHoldsSummaryAndPostBoundaryOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, provider := range []string{"claude", "codex", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			_, _, runtimeID, daemonID := compactedDM(t, provider)
			got := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
			if got.PriorSessionID != "" {
				t.Fatalf("prior session = %q, want none", got.PriorSessionID)
			}
			block, current := forkBlock(t, got.ChatMessage)
			if current != "CURRENT-INPUT" {
				t.Fatalf("current = %q", current)
			}
			for _, want := range []string{compactionSummaryText, "POST-ONE", "POST-TWO", "multica chat history"} {
				if !strings.Contains(block, want) {
					t.Fatalf("seed lacks %q:\n%s", want, block)
				}
			}
			if strings.Contains(block, "PRE-BOUNDARY") || strings.Contains(block, "CURRENT-INPUT") {
				t.Fatalf("seed holds pre-boundary text or the current input:\n%s", block)
			}
			if strings.Index(block, "POST-ONE") > strings.Index(block, "POST-TWO") || strings.Index(block, compactionSummaryText) > strings.Index(block, "POST-ONE") {
				t.Fatalf("seed is not summary then oldest-first messages:\n%s", block)
			}
		})
	}
}

// C6: a rollout-missing flag on a pre-boundary task must not raise the
// continuity-gap disclosure for the new epoch.
func TestCompactionClaim_OlderEpochRolloutMissingIsNotDisclosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, chatID, runtimeID, daemonID := compactedDM(t, "codex")
	dbfx.Exec(t, `UPDATE agent_task_queue SET session_rollout_missing = true WHERE chat_session_id = $1 AND status = 'completed'`, chatID)
	got := claimTaskForRuntimeGuard(t, runtimeID, daemonID)
	if got.PriorSessionResumeUnavailable {
		t.Fatal("prior_session_resume_unavailable set from a pre-boundary task")
	}
}
