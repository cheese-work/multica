package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/askguard"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-7138 S7a: the owner-script guard. The owner reads no shell, so an ask
// that hands the owner a command to run is a defect of the ask. One check
// writes one event, an activity_log row with action owner_script_guard, in the
// transaction of the message it judged (delivered) or on its own commit (refused).
// The event holds ids, the outcome, the rule hits and keyed hashes of the
// commands; it never holds message text.
const ownerScriptAction = "owner_script_guard"

// An R8 cue is related to a flagged message of the same author on the same
// issue sent within this window.
const ownerScriptRelatedWindow = "30 minutes"

// A refused or held command re-sent within this window is a retry of it.
const ownerScriptRetryWindow = "7 days"

const ownerScriptAlternatives = "Do not ask the owner to run shell. Use `multica config propose` for a configuration change, `multica decision propose <type>` for an approval-gated action, or ask for the bot permission you lack; link a comment without telling the owner to run it"

var ownerScriptCommentLink = regexp.MustCompile(`(?i)(?:#comment-|/comments?/)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// ownerScriptCheck is one finished check: its mode, and the event to write.
type ownerScriptCheck struct {
	AgentID, TaskID pgtype.UUID
	Event           map[string]any
}

// checkOwnerScript judges an ask before it is written. It returns nil when the
// guard is off or the call is not an ask a person reads (server-origin, batch
// and proposal cards have no agent-written text). In block mode a hit is
// refused here, with its event committed on its own; otherwise the caller
// writes the returned event in the message's transaction.
func (handler *Handler) checkOwnerScript(ctx context.Context, issue db.Issue, agentID, taskID pgtype.UUID, input decisionRequestInput) (*ownerScriptCheck, error) {
	if input.Parsed == nil || input.Proposal != nil || input.Batch != nil {
		return nil, nil
	}
	workspace, err := handler.Queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return nil, err
	}
	mode, err := askguard.OwnerScriptModeFromSettings(workspace.Settings)
	if err != nil {
		return nil, err
	}
	if mode == askguard.ModeOff {
		return nil, nil
	}
	message := decisiontx.OwnerScriptMessageFromAsk(*input.Parsed, input.Footer)
	message.Attachments = handler.ownerScriptAskAttachments(ctx, issue, input.Parsed.Artifacts)
	features := decisiontx.ExtractOwnerScript(message)
	if features.RunCue {
		if message.Related, err = handler.ownerScriptRelated(ctx, issue, agentID, message.Texts); err != nil {
			return nil, err
		}
		features = decisiontx.ExtractOwnerScript(message)
	}
	recipient := input.HumanUserID
	if !recipient.Valid && input.SessionID.Valid {
		if session, err := handler.Queries.GetChatSession(ctx, input.SessionID); err == nil {
			recipient = session.CreatorID
		}
	}
	hmacs := decisiontx.OwnerScriptCmdHMACs(decisiontx.OwnerScriptKey(auth.JWTSecret()), features.Commands)
	surface := "decision_ask"
	if input.Kind == decisiontx.KindAction {
		surface = "action_ask"
	}
	event := map[string]any{
		"rule_version": decisiontx.OwnerScriptRuleVersion, "surface": surface, "mode": string(mode),
		"outcome": "accepted", "delivered": true, "rule_hits": nonNil(features.Hits), "cmd_hmacs": nonNil(hmacs),
		"retry_of": nil, "author_agent_id": uuidToString(agentID), "task_id": uuidToString(taskID),
		"recipient_member_id": uuidToString(recipient),
	}
	if len(hmacs) > 0 {
		if prior, err := handler.ownerScriptRetryOf(ctx, issue, agentID, recipient, hmacs); err != nil {
			return nil, err
		} else if prior != "" {
			event["retry_of"] = prior
		}
	}
	check := &ownerScriptCheck{AgentID: agentID, TaskID: taskID, Event: event}
	if len(features.Hits) == 0 {
		return check, nil
	}
	event["outcome"] = "warned"
	if mode != askguard.ModeBlock {
		return check, nil
	}
	event["outcome"], event["delivered"] = "refused", false
	if err := check.write(ctx, handler.Queries, issue); err != nil {
		return nil, err
	}
	return nil, decisionIntentRefusal{http.StatusUnprocessableEntity, fmt.Sprintf("this ask tells the owner to run commands (%s; rule set v%d). %s", strings.Join(features.Where, "; "), decisiontx.OwnerScriptRuleVersion, ownerScriptAlternatives)}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// ownerScriptRelated is the R8 context: a flagged message (R1 to R7) of this
// author on this issue within the window, or a comment the text links to whose
// own content or attachments hold R1 to R7 features. The linked comment is read
// from the store, so it counts whoever wrote it and whether or not an event
// covers it.
func (handler *Handler) ownerScriptRelated(ctx context.Context, issue db.Issue, agentID pgtype.UUID, texts []string) (bool, error) {
	var linked []string
	for _, text := range texts {
		for _, match := range ownerScriptCommentLink.FindAllStringSubmatch(text, -1) {
			if id := strings.ToLower(match[1]); !slices.Contains(linked, id) && len(linked) < maxOwnerScriptLinks {
				linked = append(linked, id)
			}
		}
	}
	var related bool
	err := handler.DB.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM activity_log WHERE issue_id = $1 AND action = $2 AND details->>'author_agent_id' = $3
		  AND details->'rule_hits' ?| ARRAY['R1','R2','R3','R4','R5','R6','R7']
		  AND (created_at > now() - interval '`+ownerScriptRelatedWindow+`' OR details->>'comment_id' = ANY($4::text[])))`,
		issue.ID, ownerScriptAction, uuidToString(agentID), nonNil(linked)).Scan(&related)
	if err != nil || related {
		return related, err
	}
	for _, id := range linked {
		comment, err := handler.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{ID: parseUUID(id), WorkspaceID: issue.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		message := decisiontx.OwnerScriptMessage{Texts: []string{comment.Content}}
		attachments, err := handler.Queries.ListAttachmentsByComment(ctx, db.ListAttachmentsByCommentParams{CommentID: comment.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return false, err
		}
		for _, attachment := range attachments {
			message.Attachments = append(message.Attachments, decisiontx.OwnerScriptAttachment{Name: attachment.Filename})
		}
		if decisiontx.ExtractOwnerScript(message).Primary() {
			return true, nil
		}
	}
	return false, nil
}

// maxOwnerScriptLinks caps the comments one message makes the guard read.
const maxOwnerScriptLinks = 5

// ownerScriptHeadBytes is how much of an attachment the guard reads for "#!".
const ownerScriptHeadBytes = 256

// ownerScriptAskAttachments is what R5 reads of an ask's artifacts: the stored
// file name and the first bytes. An artifact that is not an attachment of the
// issue is skipped here; the artifact check refuses it.
func (handler *Handler) ownerScriptAskAttachments(ctx context.Context, issue db.Issue, artifacts []decisiontx.Artifact) []decisiontx.OwnerScriptAttachment {
	var files []decisiontx.OwnerScriptAttachment
	for _, artifact := range artifacts {
		attachment, err := handler.sourceIssueAttachment(ctx, issue, artifact.AttachmentID, "artifact")
		if err != nil {
			continue
		}
		file := decisiontx.OwnerScriptAttachment{Name: attachment.Filename}
		if handler.Storage != nil {
			if reader, err := handler.Storage.GetReader(ctx, handler.Storage.KeyFromURL(attachment.Url)); err == nil {
				head, _ := io.ReadAll(io.LimitReader(reader, ownerScriptHeadBytes))
				reader.Close()
				file.Head = string(head)
			}
		}
		files = append(files, file)
	}
	return files
}

// ownerScriptRetryOf is the latest refused or held event of this author to this
// person that shares a command hash: the new message retries it.
func (handler *Handler) ownerScriptRetryOf(ctx context.Context, issue db.Issue, agentID, recipient pgtype.UUID, hmacs []string) (string, error) {
	var prior string
	err := handler.DB.QueryRow(ctx, `SELECT id::text FROM activity_log WHERE workspace_id = $1 AND action = $2
		AND details->>'author_agent_id' = $3 AND details->>'recipient_member_id' = $4
		AND details->>'outcome' IN ('refused', 'held') AND details->'cmd_hmacs' ?| $5::text[]
		AND created_at > now() - interval '`+ownerScriptRetryWindow+`'
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		issue.WorkspaceID, ownerScriptAction, uuidToString(agentID), uuidToString(recipient), hmacs).Scan(&prior)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return prior, err
}

// write stores the event on queries: the caller's transaction for a delivered
// message, the plain pool for a refusal. It adds no message text.
func (check *ownerScriptCheck) write(ctx context.Context, queries *db.Queries, issue db.Issue) error {
	details, err := json.Marshal(check.Event)
	if err != nil {
		return err
	}
	_, err = queries.CreateActivity(ctx, db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "agent", Valid: true},
		ActorID: check.AgentID, Action: ownerScriptAction, Details: details,
	})
	return err
}

// writeDelivered completes the event with the ids of the committed message and
// writes it in the message's transaction.
func (check *ownerScriptCheck) writeDelivered(ctx context.Context, queries *db.Queries, issue db.Issue, request db.DecisionRequest, messageID, commentID pgtype.UUID) error {
	check.Event["request_id"] = uuidToString(request.ID)
	check.Event["message_id"] = uuidToString(messageID)
	check.Event["comment_id"] = uuidToString(commentID)
	return check.write(ctx, queries, issue)
}
