package handler

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Notices come from run messages posted to ReportTaskMessages, exactly as the
// daemon posts them. Fixtures are sanitized from Claude Code 2.1.318's
// system/model_refusal_fallback shape; the incident one is Opus 5.5 -> 4.8
// with category cyber (CHE-10140). They are fixtures, not live evidence.

var (
	modelChangePublishMu   sync.Mutex
	modelChangePublishes   = map[string][]protocol.ChatMessagePayload{}
	modelChangeSubscribeMu sync.Once
)

func modelChangePublished(t *testing.T, sessionID string) []protocol.ChatMessagePayload {
	t.Helper()
	modelChangeSubscribeMu.Do(func() {
		testHandler.Bus.Subscribe(protocol.EventChatMessage, func(e events.Event) {
			p, ok := e.Payload.(protocol.ChatMessagePayload)
			if !ok || p.MessageKind != protocol.ChatMessageKindModelChangeNotice {
				return
			}
			modelChangePublishMu.Lock()
			modelChangePublishes[p.ChatSessionID] = append(modelChangePublishes[p.ChatSessionID], p)
			modelChangePublishMu.Unlock()
		})
	})
	modelChangePublishMu.Lock()
	defer modelChangePublishMu.Unlock()
	return append([]protocol.ChatMessagePayload(nil), modelChangePublishes[sessionID]...)
}

func modelChangeEvent(seq int, key string, at time.Time, input map[string]any) map[string]any {
	return map[string]any{
		"seq": seq, "type": "model_change", "content": "Model change",
		"input": input, "idempotency_key": key, "created_at": at.UTC().Format(time.RFC3339Nano),
	}
}

func incidentInput(nativeID, phase string) map[string]any {
	return map[string]any{
		"runtime": "claude", "event": "model_refusal_fallback", "phase": phase,
		"source_model": "claude-opus-5-5", "target_model": "claude-opus-4-8",
		"reason": "refusal", "category": "cyber", "direction": "retry", "scope": "session",
		"native_event_id": nativeID,
	}
}

func (f compactNoticeFixture) modelNotices(t *testing.T) []noticeRow {
	t.Helper()
	rows, err := dbfx.Pool.Query(context.Background(),
		`SELECT id::text, content, created_at, task_id::text FROM chat_message
		 WHERE chat_session_id = $1 AND message_kind = 'model_change_notice' ORDER BY created_at, id`, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []noticeRow
	for rows.Next() {
		var r noticeRow
		if err := rows.Scan(&r.id, &r.content, &r.createdAt, &r.taskID); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (f compactNoticeFixture) wantModelNotices(t *testing.T, n int) []noticeRow {
	t.Helper()
	got := f.modelNotices(t)
	if len(got) != n {
		t.Fatalf("model change notices = %d (%+v), want %d", len(got), got, n)
	}
	if p := modelChangePublished(t, f.sessionID); len(p) != n {
		t.Fatalf("chat:message model_change_notice publishes = %d, want %d", len(p), n)
	}
	return got
}

func TestReportTaskMessagesWritesModelChangeNoticeForIncidentShape(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID) // registers the subscriber
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	f.report(t, modelChangeEvent(1, "m1", at, incidentInput("evt-1", "attempt")))

	got := f.wantModelNotices(t, 1)
	want := "Runtime is retrying Opus 5.5 → Opus 4.8. Reason: provider safety classification cyber. Scope: session."
	if got[0].content != want {
		t.Fatalf("content = %q, want %q", got[0].content, want)
	}
	if !got[0].createdAt.Equal(at) || got[0].taskID != nil {
		t.Fatalf("created_at = %v (want %v), task_id = %v (want none)", got[0].createdAt, at, got[0].taskID)
	}
	pub := modelChangePublished(t, f.sessionID)[0]
	if pub.Role != "assistant" || pub.Content != want || pub.MessageID != got[0].id {
		t.Fatalf("published = %+v", pub)
	}
	// The configured agent model is never touched by a runtime transition.
	if n := dbfx.Count(t, `SELECT count(*) FROM agent WHERE id = $1 AND model <> ''`, f.agentID); n != 0 {
		t.Fatalf("agent model changed by a runtime fallback")
	}
}

func TestReportTaskMessagesReplayedNativeModelEventWritesOneNotice(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID)
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	// Same native event reported twice under different run-message keys, as a
	// resumed or replayed stream would.
	f.report(t, modelChangeEvent(1, "m1", at, incidentInput("evt-1", "attempt")))
	f.report(t, modelChangeEvent(2, "m2", at.Add(time.Second), incidentInput("evt-1", "attempt")))
	f.wantModelNotices(t, 1)
}

func TestReportTaskMessagesKeepsGenuineAToBToATransitions(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID)
	base := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	back := incidentInput("evt-2", "return")
	back["source_model"], back["target_model"] = "claude-opus-4-8", "claude-opus-5-5"
	f.report(t, modelChangeEvent(1, "m1", base, incidentInput("evt-1", "attempt")))
	f.report(t, modelChangeEvent(2, "m2", base.Add(time.Minute), back))
	f.report(t, modelChangeEvent(3, "m3", base.Add(2*time.Minute), incidentInput("evt-3", "attempt")))

	got := f.wantModelNotices(t, 3)
	if got[1].content != "Runtime returned to Opus 5.5 from Opus 4.8. Scope: session." {
		t.Fatalf("middle notice = %q", got[1].content)
	}
}

func TestReportTaskMessagesDelayedModelEventDoesNotOvertakeNewerOnes(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID)
	base := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	confirmed := incidentInput("evt-1:confirmed", "confirmed")
	// The newer confirmation is delivered first; the older attempt arrives late.
	f.report(t, modelChangeEvent(2, "m2", base.Add(time.Minute), confirmed))
	f.report(t, modelChangeEvent(1, "m1", base, incidentInput("evt-1", "attempt")))

	got := f.wantModelNotices(t, 2)
	if got[0].content[:10] != "Runtime is" || got[1].content[:10] != "Confirmed:" {
		t.Fatalf("history must read by event time, got %q then %q", got[0].content, got[1].content)
	}
}

func TestReportTaskMessagesSubagentOnlyChangeLeavesMainModelUnchanged(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID)
	in := incidentInput("evt-local", "attempt")
	in["scope"] = "local"
	f.report(t, modelChangeEvent(1, "m1", time.Now().UTC().Add(time.Second).Truncate(time.Microsecond), in))

	got := f.wantModelNotices(t, 1)
	want := "A subagent or side request is retrying Opus 5.5 → Opus 4.8. Reason: provider safety classification cyber. Scope: subagent only; the DM's model is unchanged."
	if got[0].content != want {
		t.Fatalf("content = %q", got[0].content)
	}
}

func TestReportTaskMessagesFailedFallbackDoesNotClaimASwitch(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID)
	base := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	failed := incidentInput("evt-1:failed", "failed")
	f.report(t, modelChangeEvent(1, "m1", base, incidentInput("evt-1", "attempt")))
	f.report(t, modelChangeEvent(2, "m2", base.Add(time.Second), failed))

	got := f.wantModelNotices(t, 2)
	if want := "The retry on Opus 4.8 failed before it responded, so no switch happened. Serving model: unknown. Scope: session."; got[1].content != want {
		t.Fatalf("content = %q", got[1].content)
	}
}

func TestReportTaskMessagesNoModelNoticeOutsideWebChatTurns(t *testing.T) {
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	t.Run("compact task", func(t *testing.T) {
		f := newCompactNoticeChat(t, "compact")
		f.report(t, modelChangeEvent(1, "m1", at, incidentInput("evt-1", "attempt")))
		f.wantModelNotices(t, 0)
	})
	t.Run("channel-bound DM", func(t *testing.T) {
		f := newCompactNoticeChat(t, "")
		seedChatSourceBinding(t, f.agentID, f.sessionID)
		f.report(t, modelChangeEvent(1, "m1", at, incidentInput("evt-1", "attempt")))
		f.wantModelNotices(t, 0)
	})
}

func TestModelChangeNoticeKindReachesTheAPI(t *testing.T) {
	t.Parallel()
	if got := normalizeMessageKind(protocol.ChatMessageKindModelChangeNotice); got != protocol.ChatMessageKindModelChangeNotice {
		t.Fatalf("API kind = %q", got)
	}
}

// Producer -> parser -> server: a fake Claude CLI streams the incident-shaped
// frames; the parsed run messages are posted as the daemon would and the DM
// ends with one attempt and one confirmed notice, nothing else.
func TestClaudeStreamFallbackProducesDMNotices(t *testing.T) {
	f := newCompactNoticeChat(t, "")
	modelChangePublished(t, f.sessionID)
	frames := `{"type":"system","subtype":"init","session_id":"s1"}
{"type":"system","subtype":"model_refusal_fallback","uuid":"11111111-1111-4111-8111-111111111111","session_id":"s1","trigger":"refusal","direction":"retry","scope":"session","original_model":"claude-opus-5-5","fallback_model":"claude-opus-4-8","request_id":"req_1","api_refusal_category":"cyber"}
{"type":"assistant","session_id":"s1","message":{"id":"m2","model":"claude-opus-4-8","content":[{"type":"text","text":"ok"}]}}
{"type":"result","subtype":"success","session_id":"s1","result":"ok"}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "frames.jsonl"), []byte(frames), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nIFS= read -r _\ncat "+filepath.Join(dir, "frames.jsonl")+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := agent.New("claude", agent.Config{ExecutablePath: fake, Env: map[string]string{"IS_SANDBOX": "1"}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt", agent.ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var batch []any
	at := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	for m := range session.Messages {
		if m.Type == agent.MessageModelChange {
			batch = append(batch, modelChangeEvent(len(batch)+1, "k"+string(rune('a'+len(batch))), at.Add(time.Duration(len(batch))*time.Second), m.Input))
		}
	}
	<-session.Result
	f.report(t, batch...)

	got := f.wantModelNotices(t, 2)
	if got[0].content != "Runtime is retrying Opus 5.5 → Opus 4.8. Reason: provider safety classification cyber. Scope: session." ||
		got[1].content != "Confirmed: Opus 4.8 served a response (requested Opus 5.5). Scope: session." {
		t.Fatalf("notices = %q / %q", got[0].content, got[1].content)
	}
}
