package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5682 tests 11-14 at the wiring level: a stored action row feeds the match
// key the same way a new draft does, and only a rejected, unrevoked action
// stands. The key rules themselves are layer 1's tests.

func decidedActionRow(t *testing.T, ask decisiontx.Ask, params string, slots []decisionaction.SecretSlot, state string) db.ListDecidedAskCandidatesRow {
	t.Helper()
	askJSON, err := json.Marshal(ask)
	if err != nil {
		t.Fatal(err)
	}
	slotJSON, err := json.Marshal(slots)
	if err != nil {
		t.Fatal(err)
	}
	return db.ListDecidedAskCandidatesRow{
		ID: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}, Status: "answered", AskJson: askJSON, EscalationClass: "deploy",
		AnswerID: pgtype.UUID{Bytes: [16]byte{2}, Valid: true}, AnswerSource: "card", ChosenOptionIndex: pgtype.Int4{Int32: 0, Valid: true},
		AnswerEffectiveAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ActionID:          pgtype.UUID{Bytes: [16]byte{3}, Valid: true}, ActionType: pgtype.Text{String: "set_flag", Valid: true},
		ActionVersion: pgtype.Int4{Int32: 1, Valid: true}, ActionParams: []byte(params), ActionSecretSlots: slotJSON,
		ActionState: pgtype.Text{String: state, Valid: true},
	}
}

func TestDecidedAskActionWiring(t *testing.T) {
	ask := newSingleCallFixture(t).details
	slots := []decisionaction.SecretSlot{{Slot: "token"}}
	draft := func(mutate func(*decisiontx.ActionKeyInput)) *decisiontx.ActionKeyInput {
		action := &decisiontx.ActionKeyInput{Type: "set_flag", Version: 1, Params: json.RawMessage(`{"a":1,"b":2}`), SecretSlots: slots}
		if mutate != nil {
			mutate(action)
		}
		return action
	}
	row := decidedActionRow(t, ask, `{"b":2,"a":1}`, slots, "rejected")
	subject, err := decisiontx.SubjectKey(decisiontx.KindDecision, ask)
	if err != nil {
		t.Fatal(err)
	}
	outcome := func(row db.ListDecidedAskCandidatesRow, action *decisiontx.ActionKeyInput) string {
		match, err := decisiontx.MatchKey(decisiontx.KindDecision, "deploy", ask, action)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := judgeDecided([]db.ListDecidedAskCandidatesRow{row}, subject, match)
		return got
	}
	for name, tc := range map[string]struct {
		row    db.ListDecidedAskCandidatesRow
		action *decisiontx.ActionKeyInput
		want   string
	}{
		"same action, params in another key order": {row, draft(nil), "match"},
		"no action in the new ask":                 {row, nil, "key_differs"},
		"changed action type":                      {row, draft(func(a *decisiontx.ActionKeyInput) { a.Type = "other" }), "key_differs"},
		"changed action version":                   {row, draft(func(a *decisiontx.ActionKeyInput) { a.Version = 2 }), "key_differs"},
		"changed secret slots":                     {row, draft(func(a *decisiontx.ActionKeyInput) { a.SecretSlots = nil }), "key_differs"},
		"approved action never matches":            {decidedActionRow(t, ask, `{"a":1,"b":2}`, slots, "approved"), draft(nil), "incomparable"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := outcome(tc.row, tc.action); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("revoked action never matches", func(t *testing.T) {
		revoked := row
		revoked.ActionRevokedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		if got := outcome(revoked, draft(nil)); got != "incomparable" {
			t.Fatalf("outcome %q, want incomparable", got)
		}
	})
}
