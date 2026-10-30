package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Next-turn fallback for PR events (CHE-2298 a3). An event no mid-run delivery
// takes (no capability, no live run, a claim that lost the race with turn end)
// is handed to the issue's pr_needs_attention rule as a receipt: the rule keeps
// at most one pending wake, and the receipts it holds are the causes that wake
// lists. The event stays next_turn until the carrying run starts; a wake that
// ends unstarted, or a receipt the rule discarded, hands the cause back here.
const (
	wakeupPREventType    = "pr.event"
	wakeupPREventKeyBase = "pr_event:"
	wakeupPREventBatch   = 100
)

// prEventCursor makes the bridge walk the eligible events in batches. Events a
// rule cannot take now (switched off, held) keep no receipt, so without a
// cursor the same oldest batch would be selected on every pass and starve every
// later event. The cursor lives in memory: a restart begins a new walk.
type prEventCursor struct {
	mu sync.Mutex
	at pgtype.Timestamptz
	id pgtype.UUID
}

func (c *prEventCursor) position() (pgtype.Timestamptz, pgtype.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at, c.id
}

func (c *prEventCursor) advance(at pgtype.Timestamptz, id pgtype.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at, c.id = at, id
}

func (c *prEventCursor) rewind() { c.advance(pgtype.Timestamptz{}, pgtype.UUID{}) }

// queueNextTurnPREvents settles started wakes, then bridges every event no
// receipt is carrying. It follows TriggerPullRequestWakeup: same issue and
// configuration checks, same rebase, so a cause is never recorded under a
// revision dispatch would retire. A rule that is off or held records nothing
// but a visible note; the event stays stored for when the rule is back.
func (s *IssueWakeupService) queueNextTurnPREvents(ctx context.Context, workspaceIDs []pgtype.UUID) error {
	if _, err := s.Tasks.Queries.MarkStartedPREventsDelivered(ctx, workspaceIDs); err != nil {
		return err
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	afterAt, afterID := s.prEvents.position()
	events, err := q.ListNextTurnPREvents(ctx, db.ListNextTurnPREventsParams{
		WorkspaceIds: workspaceIDs, RowLimit: wakeupPREventBatch, AfterObservedAt: afterAt, AfterID: afterID,
	})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		s.prEvents.rewind()
		return nil
	}
	var withdrawn []db.AgentTaskQueue
	var noted []wakeupActivity
	for _, e := range events {
		tasks, activity, err := s.bridgePREvent(ctx, tx, q, e)
		if err != nil {
			return err
		}
		withdrawn = append(withdrawn, tasks...)
		noted = append(noted, activity...)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(events) < wakeupPREventBatch {
		s.prEvents.rewind()
	} else {
		last := events[len(events)-1]
		s.prEvents.advance(last.ObservedAt, last.ID)
	}
	for _, task := range withdrawn {
		s.Tasks.broadcastTaskEvent(ctx, protocol.EventTaskCancelled, task)
	}
	s.publishWakeupActivities(noted...)
	return nil
}

func (s *IssueWakeupService) bridgePREvent(ctx context.Context, tx pgx.Tx, q *db.Queries, e db.ListNextTurnPREventsRow) ([]db.AgentTaskQueue, []wakeupActivity, error) {
	var workspace pgtype.UUID
	if err := tx.QueryRow(ctx, "SELECT w.id FROM workspace w WHERE w.id=$1 FOR KEY SHARE OF w", e.WorkspaceID).Scan(&workspace); err != nil {
		return nil, nil, err
	}
	issue, err := q.LockWakeupIssue(ctx, e.IssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if active, err := systemWakeupIssueActive(ctx, q, issue, true); err != nil || !active {
		return nil, nil, err
	}
	// The event was selected before this issue lock: a legacy rule wake that
	// committed in between already carries it (CHE-2540).
	if covered, err := q.PREventCoveredByLegacyWake(ctx, e.ID); err != nil || covered {
		return nil, nil, err
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	w, err := q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue.ID, SystemRule: systemRuleText(SystemRulePRAttention)})
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, err
	}
	var legacy *db.IssueWakeup
	if exists {
		legacy = &w
	}
	config, err := loadBuiltinWakeup(ctx, q, issue, SystemRulePRAttention, legacy)
	held := false
	switch {
	case errors.Is(err, errWakeupConfigHeld):
		config, held, err = nil, true, nil
	case errors.Is(err, errMalformedPRWakeupSettings):
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	enabled, err := prRuleEnabled(ws.Settings, SystemRulePRAttention, config != nil)
	if errors.Is(err, errMalformedPRWakeupSettings) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if config != nil {
		enabled = enabled && config.enabled()
	}
	if !exists {
		w, err = q.CreateSystemWakeup(ctx, db.CreateSystemWakeupParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			EventTypes: []string{}, Enabled: true, SystemRule: systemRuleText(SystemRulePRAttention),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			w, err = q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue.ID, SystemRule: systemRuleText(SystemRulePRAttention)})
		}
		if err != nil {
			return nil, nil, err
		}
	}
	if !enabled || held || config.expired(w.CreatedAt.Time, time.Now()) {
		activity, err := s.notePREventQueued(ctx, q, w, e)
		return nil, activity, err
	}
	var withdrawn []db.AgentTaskQueue
	if w, withdrawn, err = rebaseBuiltinConfig(ctx, tx, q, issue, w, config); err != nil {
		return nil, nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"event_id": util.UUIDToString(e.ID), "kind": e.Kind, "pr_number": e.PrNumber,
		"repo_owner": e.RepoOwner, "repo_name": e.RepoName, "head_sha": e.HeadSha, "base_sha": e.BaseSha,
		"old_head_sha": e.OldHeadSha, "check_name": e.CheckName,
	})
	if err != nil {
		return nil, nil, err
	}
	// A key per attempt: a receipt the rule discarded or a wake that never
	// started must not hide the cause from the next attempt.
	if _, err := q.RecordWakeupReceipt(ctx, db.RecordWakeupReceiptParams{
		ID: dbid.NewV7(), WakeupID: w.ID, Revision: w.Revision,
		EventKey: wakeupPREventKeyBase + util.UUIDToString(e.ID) + ":" + util.UUIDToString(dbid.NewV7()), EventType: wakeupPREventType, Payload: payload,
	}); err != nil {
		return nil, nil, err
	}
	return withdrawn, nil, q.MarkPREventNextTurn(ctx, e.ID)
}

// notePREventQueued keeps a cause the rule cannot take now on the timeline,
// once per event.
func (s *IssueWakeupService) notePREventQueued(ctx context.Context, q *db.Queries, w db.IssueWakeup, e db.ListNextTurnPREventsRow) ([]wakeupActivity, error) {
	id := util.UUIDToString(e.ID)
	if done, err := q.PREventQueuedNoted(ctx, db.PREventQueuedNotedParams{IssueID: e.IssueID, EventID: id}); err != nil || done {
		return nil, err
	}
	a, err := recordWakeupActivity(ctx, q, w, wakeupActivityTriggered, "system", pgtype.UUID{}, map[string]any{
		"rule": SystemRulePRAttention, "outcome": wakeupOutcomeQueuedNextTurn, "event_id": id,
		"message": fmt.Sprintf("PR event queued for next turn: %s on PR #%d", e.Kind, e.PrNumber),
	})
	return []wakeupActivity{a}, err
}

// prEventCause names one pending PR fact for the "queued for next turn" activity.
func prEventCause(r db.IssueWakeupReceipt) string {
	var p struct {
		Kind     string `json:"kind"`
		PRNumber int32  `json:"pr_number"`
	}
	_ = json.Unmarshal(r.Payload, &p)
	if p.Kind == "" {
		p.Kind = r.EventType
	}
	return fmt.Sprintf("%s on PR #%d", p.Kind, p.PRNumber)
}

// maxPREventSentences bounds the instruction of a wake that carries many
// causes; the trigger facts list every one of them.
const maxPREventSentences = 5

// prAttentionInstruction words the instruction of a pr_needs_attention wake from
// every cause it carries, one sentence per distinct cause. A pr.event cause is
// worded by its kind from its own receipt, so each names its own PR and a field
// the event does not carry is left out with its clause. Any other receipt is a
// mergeability or returned-to-draft cause and keeps its own text. A wake with
// no receipt falls back to the latest facts.
func prAttentionInstruction(receipts []db.IssueWakeupReceipt, pr string, facts map[string]any) string {
	var sentences []string
	seen := map[string]bool{}
	add := func(key, sentence string) {
		if !seen[key] {
			seen[key] = true
			sentences = append(sentences, sentence)
		}
	}
	// notMerge is set by any cause that says the PR cannot merge; legacy marks
	// causes that bring their own closing guidance.
	notMerge, legacy := false, false
	for _, r := range receipts {
		if r.EventType != wakeupPREventType {
			var own map[string]any
			if json.Unmarshal(r.Payload, &own) != nil {
				continue
			}
			number, _ := own["pr_number"].(float64)
			url, _ := own["pr_url"].(string)
			label := prLabel(int64(number), url)
			if number == 0 {
				label = pr
			}
			legacy = true
			notMerge = notMerge || own["reason"] != PRAttentionDraft
			add(fmt.Sprintf("%s|%v|%v|%v|%v", r.EventType, number, own["reason"], own["merge_state"], own["head_sha"]), prAttentionLegacyText(label, own))
			continue
		}
		var p struct {
			Kind       string `json:"kind"`
			PRNumber   int64  `json:"pr_number"`
			RepoOwner  string `json:"repo_owner"`
			RepoName   string `json:"repo_name"`
			HeadSHA    string `json:"head_sha"`
			BaseSHA    string `json:"base_sha"`
			OldHeadSHA string `json:"old_head_sha"`
			CheckName  string `json:"check_name"`
		}
		if json.Unmarshal(r.Payload, &p) != nil {
			continue
		}
		notMerge = notMerge || p.Kind == "conflict"
		sentence := prEventSentence(prLabel(p.PRNumber, ""), p.Kind, p.HeadSHA, p.BaseSHA, p.OldHeadSHA, p.CheckName)
		add(p.RepoOwner+"/"+p.RepoName+"|"+sentence, sentence)
	}
	if len(sentences) == 0 {
		return prAttentionLegacyText(pr, facts)
	}
	text := strings.Join(sentences[:min(len(sentences), maxPREventSentences)], " ")
	if more := len(sentences) - maxPREventSentences; more > 0 {
		text += fmt.Sprintf(" %d more PR events are in the trigger facts.", more)
	}
	// The guard rides on the causes, not on which sentences fit: a conflict
	// never loses "Do not merge" to a draft cause or to the sentence cap.
	switch {
	case notMerge && !strings.Contains(text, "Do not merge"):
		text += " Update the branch or resolve the conflicts, or say on this issue who owns that. Do not merge."
	case !notMerge && !legacy:
		text += " Read the current PR and issue state, then decide what follow-up is needed."
	}
	return text
}

func prLabel(number int64, url string) string {
	label := fmt.Sprintf("PR #%d", number)
	if url != "" {
		label += " (" + url + ")"
	}
	return label
}

func prEventSentence(pr, kind, head, base, oldHead, check string) string {
	onHead := ""
	if head != "" {
		onHead = " on head " + head
	}
	switch kind {
	case "head_changed":
		text := "Linked " + pr + " has a new head"
		if head != "" {
			text += ": " + head
		}
		if oldHead != "" {
			text += ", replacing " + oldHead
		}
		return text + "."
	case "conflict":
		text := "Linked " + pr + " has a merge conflict" + onHead
		if base != "" {
			text += " against base " + base
		}
		return text + "."
	case "checks_failed":
		text := "Checks for linked " + pr + " failed" + onHead
		if check != "" {
			text += "; failing check: " + check
		}
		return text + "."
	default:
		if kind == "" {
			kind = "event"
		}
		return "Linked " + pr + " reported " + kind + onHead + "."
	}
}
