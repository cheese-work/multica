package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// modelChangeNoticeNamespace scopes the UUIDv5 ids of model change notices.
var modelChangeNoticeNamespace = uuid.MustParse("5c1f0a6e-3b7d-4d52-9e0a-6f2d4c8b1a37")

// modelChangeNoticeID derives the notice id. A native event identity (runtime +
// event id + phase) is stable across replays of the same event, so a replay
// writes no second notice, while a later A→B→A transition has its own event id.
// Without a native identity, each run message is its own notice.
func modelChangeNoticeID(sessionID, runMessageID pgtype.UUID, input map[string]any) pgtype.UUID {
	str := func(k string) string { s, _ := input[k].(string); return s }
	key := "run:" + uuid.UUID(runMessageID.Bytes).String()
	if native := str("native_event_id"); native != "" {
		key = "native:" + str("runtime") + ":" + native + ":" + str("phase")
	}
	return pgtype.UUID{Bytes: uuid.NewSHA1(modelChangeNoticeNamespace, []byte(uuid.UUID(sessionID.Bytes).String()+"|"+key)), Valid: true}
}

var modelVersionPart = regexp.MustCompile(`^\d{1,2}$`)

// modelDisplayName shortens a Claude model id ("claude-opus-5-5" → "Opus 5.5").
// Any other id is shown exactly as the runtime reported it.
func modelDisplayName(id string) string {
	rest, ok := strings.CutPrefix(id, "claude-")
	if !ok {
		return id
	}
	parts := strings.Split(rest, "-")
	family := parts[0]
	var version []string
	for _, p := range parts[1:] {
		if !modelVersionPart.MatchString(p) {
			break // a date suffix or an unknown tail ends the version
		}
		version = append(version, p)
	}
	if family == "" || len(version) == 0 {
		return id
	}
	return strings.ToUpper(family[:1]) + family[1:] + " " + strings.Join(version, ".")
}

func modelChangeReason(reason, category string) string {
	switch reason {
	case "":
		if category != "" {
			return "provider safety classification " + category
		}
		return "not reported"
	case "refusal":
		return strings.TrimSpace("provider safety classification " + category)
	case "overloaded":
		return "provider overloaded"
	case "model_not_found":
		return "model not found or unavailable"
	case "permission_denied":
		return "account lacks access to the model"
	case "server_error":
		return "provider server error"
	case "last_resort":
		return "non-retryable error on the primary model"
	case "model_blocked":
		return "model disabled by the provider"
	}
	return "provider-reported " + reason
}

// modelChangeNoticeText is the DM line for one `model_change` run message. The
// wording keeps what the runtime said it would do (attempt, sticky, return)
// apart from what was observed (confirmed) and from a failed fallback, which
// never claims a switch. Requested and effective models stay distinct.
func modelChangeNoticeText(in map[string]any) string {
	str := func(k string) string { s, _ := in[k].(string); return s }
	source, target := modelDisplayName(str("source_model")), modelDisplayName(str("target_model"))
	reason := modelChangeReason(str("reason"), str("category"))
	scope := "session"
	subject := "Runtime"
	if str("scope") == "local" {
		scope = "subagent only; the DM's model is unchanged"
		subject = "A subagent or side request"
	}
	switch str("phase") {
	case "sticky":
		if str("scope") == "local" {
			return fmt.Sprintf("%s keeps %s instead of %s. Reason: %s. Scope: %s.", subject, target, source, reason, scope)
		}
		return fmt.Sprintf("Runtime keeps %s instead of %s for this session. Reason: %s. Scope: %s.", target, source, reason, scope)
	case "return":
		return fmt.Sprintf("%s returned to %s from %s. Scope: %s.", subject, target, source, scope)
	case "confirmed":
		return fmt.Sprintf("Confirmed: %s served a response (requested %s). Scope: %s.", target, source, scope)
	case "failed":
		if str("target_model") == "" {
			return fmt.Sprintf("%s refused the request (%s) and no fallback model was available.", source, reason)
		}
		return fmt.Sprintf("The retry on %s failed before it responded, so no switch happened. Serving model: unknown. Scope: %s.", target, scope)
	}
	return fmt.Sprintf("%s is retrying %s → %s. Reason: %s. Scope: %s.", subject, source, target, reason, scope)
}

// ReconcileModelChangeNotices projects every `model_change` run message of a
// web DM's chat task to one `model_change_notice` in the DM (CHE-10140). Like
// ReconcileCompactNotices it is idempotent and works whatever the task status.
// Rows sit at the event time, so a delayed message lands before newer ones and
// never rewrites them: the effective model is read from the ordered, immutable
// notices, never stored as mutable state.
func (s *TaskService) ReconcileModelChangeNotices(ctx context.Context, taskID pgtype.UUID) ([]db.ChatMessage, error) {
	return s.reconcileRunNotices(ctx, taskID, func(qtx *db.Queries, sessionID pgtype.UUID) ([]db.ChatMessage, error) {
		runMessages, err := qtx.ListTaskModelChangeMessages(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("list model_change messages: %w", err)
		}
		var created []db.ChatMessage
		for _, event := range runMessages {
			var input map[string]any
			if err := json.Unmarshal(event.Input, &input); err != nil || input == nil {
				continue // unreadable evidence is not announced
			}
			notice, err := qtx.CreateModelChangeNoticeIdempotent(ctx, db.CreateModelChangeNoticeIdempotentParams{
				ID:            modelChangeNoticeID(sessionID, event.ID, input),
				ChatSessionID: sessionID,
				Content:       modelChangeNoticeText(input),
				EventAt:       event.CreatedAt,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("write model change notice: %w", err)
			}
			created = append(created, notice)
		}
		return created, nil
	})
}
