package handler

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Typed refusals for an answer to a decision that is no longer live (CHE-3555,
// plan M17/M18). The code prefix is the contract; the view an answer came from,
// archived history included, never changes which one applies.
var (
	errAnswerUnauthorized = decisionIntentRefusal{http.StatusForbidden, "unauthorized_answer: only the request's human can answer"}
	errAnswerUsed         = decisionIntentRefusal{http.StatusConflict, "used_answer: this decision already has an answer"}
	errAnswerExpired      = decisionIntentRefusal{http.StatusConflict, "expired_answer: this decision expired; expiry is not an answer"}
	errAnswerSuperseded   = decisionIntentRefusal{http.StatusConflict, "superseded_answer: this decision was withdrawn or replaced; open the live decision"}
)

// refuseDeadAnswer checks one answer against the live decision row.
func refuseDeadAnswer(decision db.DecisionRequest, userID pgtype.UUID, input decisionCardAnswerInput, now time.Time) error {
	switch {
	case decision.HumanUserID != userID:
		return errAnswerUnauthorized
	case decision.Status == "answered" || decision.Status == "disclaimed":
		return errAnswerUsed
	case decision.Status == "cancelled" && decision.CancelReason.String == "expired":
		return errAnswerExpired
	case decision.Status == "cancelled":
		return errAnswerSuperseded
	case decision.ExpiresAt.Valid && !decision.ExpiresAt.Time.After(now):
		return errAnswerExpired
	case input.AnswerRevision != nil && *input.AnswerRevision != decision.AnswerRevision:
		return errStaleAnswer
	}
	return nil
}
