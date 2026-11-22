package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// decision_reask_thread (CHE-6433, thread-focus plan r3 S7): the re-ask gate.
// An agent that asks a member again on an issue names the earlier card
// (--reask-of) or says the matter is new (--new-topic). off is the default.
const (
	reaskThreadKey      = "decision_reask_thread"
	reaskThreadOff      = "off"
	reaskThreadWarn     = "warn"
	reaskThreadBlock    = "block"
	reaskThreadAskEvent = "decision_reask_thread"
)

// reaskThreadMode reads decision_reask_thread from a workspace.settings value.
// A missing key is off. A stored value the validator would have refused reads as
// off: an ask must never fail because of a setting.
func reaskThreadMode(raw []byte) string {
	if len(raw) == 0 {
		return reaskThreadOff
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return reaskThreadOff
	}
	var mode string
	if err := json.Unmarshal(settings[reaskThreadKey], &mode); err != nil || !validReaskThreadMode(mode) {
		return reaskThreadOff
	}
	return mode
}

func validReaskThreadMode(mode string) bool {
	return mode == reaskThreadOff || mode == reaskThreadWarn || mode == reaskThreadBlock
}

// validateReaskThreadSetting checks decision_reask_thread of a workspace
// settings object. An absent key is valid; null is not.
func validateReaskThreadSetting(value any) error {
	config, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	v, present := config[reaskThreadKey]
	if !present {
		return nil
	}
	if mode, _ := v.(string); !validReaskThreadMode(mode) {
		return fmt.Errorf("%s must be off, warn or block", reaskThreadKey)
	}
	return nil
}

const (
	reaskSupersededPrefix = "Superseded by re-ask "
	reaskQuestionRunes    = 80
)

// reaskGateError is the refusal of the block gate. Op is the guard row the
// caller writes after it rolls the request transaction back.
type reaskGateError struct {
	decisionCodedRefusal
	Op askguard.Op
}

func (err reaskGateError) Unwrap() error { return err.decisionCodedRefusal }

// resolveReask reads the card a --reask-of names, before the request
// transaction, and aims the new ask at that card's thread root. The card is
// read again under its lock in singleCallDecisionIn.
func (handler *Handler) resolveReask(ctx context.Context, issue db.Issue, input decisionRequestInput) (decisionRequestInput, error) {
	if !input.ReaskOf.Valid {
		return input, nil
	}
	if !input.HumanUserID.Valid {
		return input, decisionIntentRefusal{http.StatusBadRequest, "reask_of needs for: the person the earlier card asked"}
	}
	reask, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: input.ReaskOf})
	if errors.Is(err, pgx.ErrNoRows) {
		return input, decisionCodedRefusal{http.StatusNotFound, "reask_not_found", "reask_of names no decision request: " + uuidToString(input.ReaskOf)}
	}
	if err != nil {
		return input, err
	}
	switch {
	case reask.IssueID != issue.ID:
		return input, decisionCodedRefusal{http.StatusBadRequest, "reask_issue_mismatch", "reask_of names a card of another issue; re-ask on that issue"}
	case isServerDecision(reask):
		return input, decisionCodedRefusal{http.StatusConflict, "reask_server_request", "reask_of names a request the platform opened; it cannot be re-asked"}
	case reask.HumanUserID != input.HumanUserID:
		return input, decisionCodedRefusal{http.StatusBadRequest, "reask_target_mismatch", "reask_of names a card asked of another person; use --new-topic to ask this person on a new thread"}
	}
	input.ReaskSession = reask.ChatSessionID
	if reask.IssueCommentID.Valid {
		root, err := handler.Queries.GetThreadRoot(ctx, db.GetThreadRootParams{CommentID: reask.IssueCommentID, WorkspaceID: issue.WorkspaceID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return input, err
		}
		if err == nil {
			input.ParentID = root.ID
		}
	}
	return input, nil
}

// lockReask locks the card of a re-ask, after the member lock. A card a re-ask
// already replaced refuses with the successor's id: two re-asks of one card
// never both land. An open card is returned for insertDecisionAsk to withdraw.
func lockReask(ctx context.Context, queries *db.Queries, issue db.Issue, input decisionRequestInput) (decisionRequestInput, error) {
	if !input.ReaskOf.Valid {
		return input, nil
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: input.ReaskOf})
	if errors.Is(err, pgx.ErrNoRows) {
		return input, decisionCodedRefusal{http.StatusNotFound, "reask_not_found", "reask_of names no decision request: " + uuidToString(input.ReaskOf)}
	}
	if err != nil {
		return input, err
	}
	if current.Status == "cancelled" && strings.HasPrefix(current.WithdrawReason, reaskSupersededPrefix) {
		successor := strings.TrimPrefix(current.WithdrawReason, reaskSupersededPrefix)
		return input, decisionCodedRefusal{http.StatusConflict, "reask_superseded", "this card was already re-asked; the newer card is " + successor + ". Use --reask-of " + successor}
	}
	if current.Status == "open" && current.Origin == "asked" {
		input.Reask = &current
	}
	return input, nil
}

// reaskGate is the decision_reask_thread gate (CHE-6433). It applies to an
// issue ask that names neither --reask-of nor --new-topic when this agent
// already sent this member a card on this issue in the last 30 days, any status.
// It returns the hint of warn mode; block returns a reaskGateError.
func reaskGate(ctx context.Context, queries *db.Queries, issue db.Issue, input decisionRequestInput, task db.AgentTaskQueue) (string, error) {
	if input.ReaskOf.Valid || input.NewTopic || input.Proposal != nil || input.Batch != nil || input.dmSession().Valid {
		return "", nil
	}
	workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return "", err
	}
	mode := reaskThreadMode(workspace.Settings)
	if mode == reaskThreadOff {
		return "", nil
	}
	cards, err := queries.ListDecisionReaskGateCards(ctx, db.ListDecisionReaskGateCardsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AskingAgentID: task.AgentID, HumanUserID: input.HumanUserID})
	if err != nil || len(cards) == 0 {
		return "", err
	}
	member, err := queries.GetUser(ctx, input.HumanUserID)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	text.WriteString("refused: " + reaskThreadAskEvent + "\n")
	text.WriteString("This issue already has decision cards from you to " + member.Name + ":\n")
	for _, card := range cards {
		fmt.Fprintf(&text, "  %s  %s  %q\n", uuidToString(card.ID), card.Status, reaskQuestion(card))
	}
	text.WriteString("Same matter (a re-ask after a rejection or a fix): add --reask-of " + uuidToString(cards[0].ID) + ". The card goes into that card's thread.\n")
	text.WriteString("A different matter: add --new-topic. The card starts its own thread.")
	op := askguard.Op{IssueID: issue.ID, TaskID: task.ID, MemberID: input.HumanUserID, Kind: reaskThreadAskEvent, Payload: []byte(uuidToString(issue.ID) + ":" + uuidToString(input.HumanUserID) + ":" + sha256Hex(string(input.AskRaw)))}
	if mode == reaskThreadBlock {
		return "", reaskGateError{decisionCodedRefusal{http.StatusConflict, reaskThreadAskEvent, text.String()}, op}
	}
	if err := (askguard.Guard{Queries: queries}).RecordWarn(ctx, issue.WorkspaceID, op); err != nil {
		return "", err
	}
	slog.Info("decision_reask_thread: card sent without --reask-of or --new-topic", "issue_id", uuidToString(issue.ID), "task_id", uuidToString(task.ID))
	return text.String(), nil
}

// reaskQuestion is the question of a stored card: the decision of its
// structured ask, else the first line of its text, cut to 80 characters.
func reaskQuestion(card db.ListDecisionReaskGateCardsRow) string {
	var ask struct {
		Decision string `json:"decision"`
	}
	question := ""
	if json.Unmarshal(card.AskJson, &ask) == nil {
		question = ask.Decision
	}
	if strings.TrimSpace(question) == "" {
		question, _, _ = strings.Cut(card.Ask, "\n")
	}
	if runes := []rune(question); len(runes) > reaskQuestionRunes {
		question = string(runes[:reaskQuestionRunes])
	}
	return question
}

// recordReaskRefused writes the refused row of a block gate, outside the
// rolled-back request transaction. A failed write is logged at INFO: the
// refusal still goes out.
func (handler *Handler) recordReaskRefused(ctx context.Context, workspaceID pgtype.UUID, gate reaskGateError) {
	if err := (askguard.Guard{Queries: handler.Queries}).RecordRefused(ctx, workspaceID, gate.Op); err != nil {
		slog.Info("decision_reask_thread: refused row failed", "error", err, "issue_id", uuidToString(gate.Op.IssueID))
	}
}
