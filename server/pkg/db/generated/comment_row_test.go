package db

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommentRowDecisionProvenance(test *testing.T) {
	requestID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	answerID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	intentID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}
	kind := pgtype.Text{String: "quote", Valid: true}
	rows := map[string]Comment{
		"create": (CreateCommentRow{DecisionRequestID: requestID, DecisionAnswerID: answerID, DecisionIntentID: intentID, DecisionCommentKind: kind}).Comment(),
		"update": (UpdateCommentRow{DecisionRequestID: requestID, DecisionAnswerID: answerID, DecisionIntentID: intentID, DecisionCommentKind: kind}).Comment(),
	}
	for name, row := range rows {
		if row.DecisionRequestID != requestID || row.DecisionAnswerID != answerID || row.DecisionIntentID != intentID || row.DecisionCommentKind != kind {
			test.Errorf("%s row discarded server provenance: %+v", name, row)
		}
	}
}
