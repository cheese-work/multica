package handler

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Decided-ask guard (CHE-5682 layer 2). A new decision request or ask that
// repeats a question the person already answered is recorded (warn) or refused
// with the record (block). Keys come from decisiontx; this file only gathers
// the candidates and judges them. Anything unclear is no match: the ask is sent.

const decidedAskKind = "decided_ask"

// decidedAskRefusal is the response of a block hit or of an event write that
// failed. It is written by writeDecisionError.
type decidedAskRefusal struct {
	status int
	body   map[string]any
}

func (refusal decidedAskRefusal) Error() string { return fmt.Sprint(refusal.body["error"]) }

// decidedCandidate is one same-subject request with its newest answer.
type decidedCandidate struct {
	row   db.ListDecidedAskCandidatesRow
	key   string
	issue db.Issue
}

// decidedAskAction is the new ask's action as key input; nil for none.
func decidedAskAction(draft *decisionActionDraft) *decisiontx.ActionKeyInput {
	if draft == nil {
		return nil
	}
	return &decisiontx.ActionKeyInput{Type: draft.Type, Version: draft.Version, Params: draft.Params, SecretSlots: draft.SecretSlots}
}

// decidedIssueChain is the issue and its ancestors, nearest first.
func decidedIssueChain(ctx context.Context, queries *db.Queries, issue db.Issue) ([]db.Issue, error) {
	chain := []db.Issue{issue}
	for cur := issue; cur.ParentIssueID.Valid && len(chain) <= maxDecisionTopicDepth; {
		parent, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: cur.ParentIssueID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return nil, err
		}
		chain = append(chain, parent)
		cur = parent
	}
	return chain, nil
}

// decidedRowKeys is the subject and match key of a stored request. ok is false
// when the row cannot be keyed: an untyped record, no ask_json, a strict decode
// failure, or an action request with no action row.
func decidedRowKeys(row db.ListDecidedAskCandidatesRow) (subject, match string, ok bool) {
	if row.Origin == "unprompted" || len(row.AskJson) == 0 {
		return "", "", false
	}
	kind := cmp.Or(row.Kind, decisiontx.KindDecision)
	ask, err := decisiontx.DecodeAskKind(row.AskJson, kind)
	if err != nil {
		return "", "", false
	}
	var action *decisiontx.ActionKeyInput
	if row.ActionID.Valid {
		action = &decisiontx.ActionKeyInput{Type: row.ActionType.String, Version: int(row.ActionVersion.Int32), Params: row.ActionParams}
		if len(row.ActionSecretSlots) > 0 {
			if err := json.Unmarshal(row.ActionSecretSlots, &action.SecretSlots); err != nil {
				return "", "", false
			}
		}
	} else if kind == decisiontx.KindAction {
		return "", "", false
	}
	if subject, err = decisiontx.SubjectKey(kind, ask); err != nil {
		return "", "", false
	}
	if match, err = decisiontx.MatchKey(kind, row.EscalationClass, ask, action); err != nil {
		return "", "", false
	}
	return subject, match, true
}

// decidedAnswerStands reports whether the newest answer of a request is a card
// choice that still stands and, for an action, a rejected action not revoked.
func decidedAnswerStands(row db.ListDecidedAskCandidatesRow) bool {
	if row.AnswerSource != "card" || !row.ChosenOptionIndex.Valid || row.AnswerDisclaimedAt.Valid || row.AnsweredByBatchRequestID.Valid {
		return false
	}
	if row.ActionID.Valid {
		return row.ActionState.String == "rejected" && !row.ActionRevokedAt.Valid
	}
	return true
}

// judgeDecided classifies the new ask against the candidates of its subject
// and returns the request it repeats, if any. outcome is the metric label.
func judgeDecided(rows []db.ListDecidedAskCandidatesRow, subject, match string) (outcome string, hit db.ListDecidedAskCandidatesRow) {
	var keyed []decidedCandidate
	var untypedNewest pgtype.Timestamptz
	for _, row := range rows {
		rowSubject, rowMatch, ok := decidedRowKeys(row)
		if !ok {
			// It could be a newer answer to the same question: only its time matters.
			if row.AnswerID.Valid && row.Status == "answered" && (!untypedNewest.Valid || row.AnswerEffectiveAt.Time.After(untypedNewest.Time)) {
				untypedNewest = row.AnswerEffectiveAt
			}
			continue
		}
		if rowSubject != subject {
			continue
		}
		if row.Status == "open" {
			return "open", hit
		}
		if row.Status != "answered" || !row.AnswerID.Valid {
			continue
		}
		keyed = append(keyed, decidedCandidate{row: row, key: rowMatch})
	}
	if len(keyed) == 0 {
		if untypedNewest.Valid {
			return "incomparable", hit
		}
		return "no_candidate", hit
	}
	slices.SortFunc(keyed, func(a, b decidedCandidate) int {
		return cmp.Or(b.row.AnswerEffectiveAt.Time.Compare(a.row.AnswerEffectiveAt.Time), bytes.Compare(b.row.ID.Bytes[:], a.row.ID.Bytes[:]))
	})
	latest := keyed[0]
	if untypedNewest.Valid && untypedNewest.Time.After(latest.row.AnswerEffectiveAt.Time) {
		return "incomparable", hit
	}
	for _, other := range keyed[1:] {
		if !other.row.AnswerEffectiveAt.Time.Equal(latest.row.AnswerEffectiveAt.Time) {
			break
		}
		if other.key != latest.key || other.row.ChosenOptionIndex != latest.row.ChosenOptionIndex || decidedAnswerStands(other.row) != decidedAnswerStands(latest.row) {
			return "incomparable", hit
		}
	}
	if !decidedAnswerStands(latest.row) {
		return "incomparable", hit
	}
	if latest.key != match {
		return "key_differs", hit
	}
	return "match", latest.row
}

// refuseDecidedAsk runs the check for a new request or ask of a person. It
// returns nil to let the card be sent and a decidedAskRefusal to refuse it.
// queries is the caller's transaction; the event goes through handler.Queries
// so it commits before the caller rolls the transaction back.
func (handler *Handler) refuseDecidedAsk(ctx context.Context, queries *db.Queries, issue db.Issue, taskID, humanID pgtype.UUID, warnOnly bool, input decisionRequestInput) error {
	if input.Proposal != nil || input.Batch != nil {
		return nil
	}
	workspace, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return err
	}
	mode, err := askguard.DecidedModeFromSettings(workspace.Settings)
	if err != nil || mode == askguard.ModeOff {
		return err
	}
	outcome, hit, chain, checkErr := handler.checkDecidedAsk(ctx, queries, issue, humanID, input)
	if checkErr != nil {
		slog.Warn("decided ask check failed", "error", checkErr, "issue_id", uuidToString(issue.ID))
	}
	handler.Metrics.RecordDecidedAskCheck(outcome)
	if outcome != "match" {
		return nil
	}
	op := askguard.Op{
		IssueID: issue.ID, TaskID: taskID, MemberID: humanID, Kind: decidedAskKind, ModeKey: askguard.DecidedSettingKey,
		Payload: []byte(uuidToString(hit.ID)), WarnOnly: warnOnly,
	}
	verdict, err := askguard.Guard{Queries: handler.Queries, Metrics: handler.Metrics}.Hit(ctx, issue.WorkspaceID, op)
	if err != nil {
		slog.Warn("decided ask guard hit failed", "error", err)
		return decidedAskRefusal{http.StatusServiceUnavailable, map[string]any{"error": "ask guard unavailable; retry the same request"}}
	}
	if !verdict.Block {
		return nil
	}
	return handler.decidedAskBlocked(ctx, queries, workspace, hit, chain)
}

// checkDecidedAsk returns the outcome label, the repeated request on a match
// and the issue chain. A non-nil error is logged and counts as "error".
func (handler *Handler) checkDecidedAsk(ctx context.Context, queries *db.Queries, issue db.Issue, humanID pgtype.UUID, input decisionRequestInput) (string, db.ListDecidedAskCandidatesRow, []db.Issue, error) {
	var none db.ListDecidedAskCandidatesRow
	kind := cmp.Or(input.Kind, decisiontx.KindDecision)
	raw := input.AskJSON
	var ask decisiontx.Ask
	var err error
	switch {
	case len(raw) > 0:
		ask, err = decisiontx.DecodeAskKind(raw, kind)
	case input.Parsed != nil:
		ask = *input.Parsed
	default:
		return "incomparable", none, nil, nil
	}
	if err != nil {
		return "incomparable", none, nil, nil
	}
	subject, err := decisiontx.SubjectKey(kind, ask)
	if err != nil {
		return "incomparable", none, nil, nil
	}
	match, err := decisiontx.MatchKey(kind, input.EscalationClass, ask, decidedAskAction(input.Action))
	if err != nil {
		return "incomparable", none, nil, nil
	}
	chain, err := decidedIssueChain(ctx, queries, issue)
	if err != nil {
		return "error", none, nil, err
	}
	ids := make([]pgtype.UUID, len(chain))
	for i, link := range chain {
		ids[i] = link.ID
	}
	rows, err := queries.ListDecidedAskCandidates(ctx, db.ListDecidedAskCandidatesParams{WorkspaceID: issue.WorkspaceID, HumanUserID: humanID, IssueIds: ids})
	if err != nil {
		return "error", none, nil, err
	}
	outcome, hit := judgeDecided(rows, subject, match)
	return outcome, hit, chain, nil
}

// decidedAskBlocked is the 409 that carries the record of the earlier answer.
func (handler *Handler) decidedAskBlocked(ctx context.Context, queries *db.Queries, workspace db.Workspace, hit db.ListDecidedAskCandidatesRow, chain []db.Issue) error {
	var asked db.Issue
	for _, link := range chain {
		if link.ID == hit.IssueID {
			asked = link
		}
	}
	const layout = "2006-01-02 15:04 ICT"
	answeredBy := uuidToString(hit.AnsweredByUserID)
	if user, err := queries.GetUser(ctx, hit.AnsweredByUserID); err == nil {
		answeredBy = cmp.Or(user.Name, answeredBy)
	}
	chosen := hit.AnsweredLabel.String
	if chosen == "" {
		// The label is stored with the answer; fall back to the option it indexes.
		var options []decisiontx.AskOption
		if json.Unmarshal(hit.Options, &options) == nil && int(hit.ChosenOptionIndex.Int32) < len(options) {
			chosen = options[hit.ChosenOptionIndex.Int32].Label
		}
	}
	identifier := fmt.Sprintf("%s-%d", workspace.IssuePrefix, asked.Number)
	return decidedAskRefusal{http.StatusConflict, map[string]any{
		"error":               fmt.Sprintf("this decision was already answered on %s at %s: %s", identifier, hit.AnswerEffectiveAt.Time.In(ictZone).Format(layout), chosen),
		"code":                "decision_already_answered",
		"decision_request_id": uuidToString(hit.ID),
		"answer_id":           uuidToString(hit.AnswerID),
		"issue":               identifier,
		"chosen_option":       chosen,
		"answered_by":         answeredBy,
		"answered_at":         hit.AnswerEffectiveAt.Time.In(ictZone).Format(layout),
		"record_link":         util.WorkspaceIssuePath(workspace.Slug, uuidToString(asked.ID)),
	}}
}

// writeDecidedAskRefusal renders a decidedAskRefusal; 503 asks the caller to retry.
func writeDecidedAskRefusal(writer http.ResponseWriter, refusal decidedAskRefusal) {
	if refusal.status == http.StatusServiceUnavailable {
		writer.Header().Set("Retry-After", "5")
	}
	writeJSON(writer, refusal.status, refusal.body)
}
