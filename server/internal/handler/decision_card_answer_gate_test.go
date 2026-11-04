package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/passkey/passkeytest"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const gatedSession = "sid-gated-1"

type answerChallenge struct {
	issuedChallenge
	AnswerRevision int32 `json:"answer_revision"`
	PolicyRevision int32 `json:"policy_revision"`
}

type gatedCard struct {
	askedRecordFixture
	test    *testing.T
	handler *Handler
	device  *passkeytest.Authenticator
	counter uint32
}

// newGatedCard is a card of the given class in a workspace whose gate blocks,
// for a human with one passkey.
func newGatedCard(test *testing.T, class string) *gatedCard {
	test.Helper()
	card := &gatedCard{askedRecordFixture: newCardFixture(test), test: test, handler: passkeyHandler(test), counter: passkeyStart}
	dbfx.Exec(test, `UPDATE decision_request SET escalation_class = $2 WHERE id = $1`, card.decision.ID, class)
	dbfx.Exec(test, `UPDATE workspace SET decision_passkey_gate = 'block', policy_revision = policy_revision + 1 WHERE id = $1`, testWorkspaceID)
	dbfx.Cleanup(test, `UPDATE workspace SET decision_passkey_gate = 'off' WHERE id = $1`, testWorkspaceID)
	card.device = enrollDevice(test, card.humanID)
	return card
}

func enrollDevice(test *testing.T, userID string) *passkeytest.Authenticator {
	test.Helper()
	device := passkeytest.New(test)
	var id string
	dbfx.QueryRow(test, `INSERT INTO user_passkey (user_id, credential_id, public_key, sign_count, flags) VALUES ($1, $2, $3, $4, 5) RETURNING id`,
		userID, device.CredentialID, device.COSEPublicKey(test), passkeyStart).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM user_passkey WHERE id = $1`, id)
	dbfx.Cleanup(test, `DELETE FROM passkey_assertion_challenge WHERE user_id = $1`, userID)
	return device
}

func (card *gatedCard) challengeRequest(userID, source, sid string, option, text string) *http.Request {
	id := uuidToString(card.decision.ID)
	request := newRequest(http.MethodPost, "/api/decision-requests/"+id+"/answer/challenge", map[string]string{"option": option, "text": text})
	request.Header.Set("X-User-ID", userID)
	request.Header.Set("X-Actor-Source", source)
	request.Header.Set("X-Session-ID", sid)
	return withURLParam(request, "id", id)
}

func (card *gatedCard) challenge(option, text string) answerChallenge {
	card.test.Helper()
	body := testutil.Call(card.test, card.handler.ChallengeDecisionAnswer, card.challengeRequest(card.humanID, "interactive_jwt", gatedSession, option, text)).Want(http.StatusOK).Text()
	var issued answerChallenge
	decodeJSON(card.test, body, &issued)
	return issued
}

func (card *gatedCard) sign(issued answerChallenge) map[string]any {
	card.test.Helper()
	return signAs(card.test, card.device, card.humanID, issued.issuedChallenge, &card.counter)
}

func signAs(test *testing.T, device *passkeytest.Authenticator, userID string, issued issuedChallenge, counter *uint32) map[string]any {
	test.Helper()
	*counter++
	user, err := uuid.Parse(userID)
	if err != nil {
		test.Fatal(err)
	}
	signed := device.Assert(test, passkeytest.Signing{
		Challenge: issued.Options.PublicKey.Challenge, RPID: passkeyRPID, Origin: passkeyOrigin,
		Flags: passkeytest.FlagUserPresent | passkeytest.FlagUserVerified, Counter: *counter, UserID: user[:],
	})
	return map[string]any{"challenge_id": issued.ChallengeID, "credential": json.RawMessage(signed)}
}

func (card *gatedCard) answer(sid string, input decisionCardAnswerInput) (decisionRecordResult, error) {
	request := card.request()
	request.Header.Set("X-Session-ID", sid)
	return card.handler.answerDecisionRequestTx(request, card.issue.WorkspaceID, card.decision.ID, parseUUID(card.humanID), input)
}

func (card *gatedCard) signed(issued answerChallenge, option, text string) decisionCardAnswerInput {
	assertion, err := json.Marshal(card.sign(issued))
	if err != nil {
		card.test.Fatal(err)
	}
	return decisionCardAnswerInput{Option: option, Text: text, AnswerRevision: issued.AnswerRevision, PolicyRevision: issued.PolicyRevision, Assertion: assertion}
}

func (card *gatedCard) wantRefusal(err error, status int, code string) {
	card.test.Helper()
	if err == nil {
		card.test.Fatalf("answer accepted, want %d %s", status, code)
	}
	if !strings.Contains(err.Error(), code) {
		card.test.Fatalf("err = %v, want %s", err, code)
	}
	if status != 0 {
		if got := refusalStatus(err); got != status {
			card.test.Fatalf("status = %d, want %d (%v)", got, status, err)
		}
	}
	if count := dbfx.Count(card.test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID); count != 0 {
		card.test.Fatalf("refused answer left %d answers", count)
	}
}

func refusalStatus(err error) int {
	var refusal decisionIntentRefusal
	var proof passkey.Refusal
	switch {
	case errors.As(err, &refusal):
		return refusal.status
	case errors.As(err, &proof):
		return proof.Status
	}
	return 0
}

func (card *gatedCard) consumed(issued answerChallenge) bool {
	var consumed bool
	dbfx.QueryRow(card.test, `SELECT consumed_at IS NOT NULL FROM passkey_assertion_challenge WHERE id = $1`, issued.ChallengeID).Scan(&consumed)
	return consumed
}

func TestGatedClassesNeedAnAssertionWhenTheGateBlocks(test *testing.T) {
	for _, class := range []string{"irreversible", "deploy", "spend", "publish"} {
		test.Run(class, func(test *testing.T) {
			card := newGatedCard(test, class)
			_, err := card.answer(gatedSession, decisionCardAnswerInput{Option: "Option A"})
			card.wantRefusal(err, http.StatusForbidden, "assertion_required")

			issued := card.challenge("Option A", "")
			result, err := card.answer(gatedSession, card.signed(issued, "Option A", ""))
			if err != nil {
				test.Fatal(err)
			}
			if result.Answer.AnswerSource != "card" || !card.consumed(issued) {
				test.Fatalf("answer %+v consumed=%v", result.Answer, card.consumed(issued))
			}
		})
	}
}

func TestUngatedAnswersNeedNoAssertion(test *testing.T) {
	for name, setup := range map[string]func(*gatedCard){
		"reversible class": func(card *gatedCard) {
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`, card.decision.ID)
		},
		"product_direction": func(card *gatedCard) {
			dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'product_direction' WHERE id = $1`, card.decision.ID)
		},
		"gate off on a gated class": func(card *gatedCard) {
			dbfx.Exec(test, `UPDATE workspace SET decision_passkey_gate = 'off' WHERE id = $1`, testWorkspaceID)
		},
	} {
		test.Run(name, func(test *testing.T) {
			card := newGatedCard(test, "spend")
			setup(card)
			if _, err := card.answer("", decisionCardAnswerInput{Option: "Option A"}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestGatedAnswerRefusesProofBoundToOtherContent(test *testing.T) {
	cases := map[string]func(card *gatedCard, issued answerChallenge) (string, decisionCardAnswerInput){
		"option swap": func(card *gatedCard, issued answerChallenge) (string, decisionCardAnswerInput) {
			return gatedSession, card.signed(issued, "Option B", "")
		},
		"option moved into text": func(card *gatedCard, issued answerChallenge) (string, decisionCardAnswerInput) {
			return gatedSession, card.signed(issued, "", "Option A")
		},
		"text added": func(card *gatedCard, issued answerChallenge) (string, decisionCardAnswerInput) {
			return gatedSession, card.signed(issued, "Option A", "and also")
		},
		"another session": func(card *gatedCard, issued answerChallenge) (string, decisionCardAnswerInput) {
			return "sid-other", card.signed(issued, "Option A", "")
		},
		"no session": func(card *gatedCard, issued answerChallenge) (string, decisionCardAnswerInput) {
			return "", card.signed(issued, "Option A", "")
		},
	}
	for name, mutate := range cases {
		test.Run(name, func(test *testing.T) {
			card := newGatedCard(test, "irreversible")
			issued := card.challenge("Option A", "")
			sid, input := mutate(card, issued)
			_, err := card.answer(sid, input)
			card.wantRefusal(err, http.StatusForbidden, "")
			if card.consumed(issued) {
				test.Fatal("a refused answer consumed the proof")
			}
		})
	}
}

func TestGatedAnswerRefusesAnotherUsersProof(test *testing.T) {
	card := newGatedCard(test, "irreversible")
	other := newGatedCard(test, "irreversible")
	issued := other.challenge("Option A", "")
	_, err := card.answer(gatedSession, other.signed(issued, "Option A", ""))
	card.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
}

func TestGatedAnswerRefusesStaleRevisionsBeforeVerifying(test *testing.T) {
	test.Run("answer revision", func(test *testing.T) {
		card := newGatedCard(test, "irreversible")
		issued := card.challenge("Option A", "")
		dbfx.Exec(test, `UPDATE decision_request SET answer_revision = answer_revision + 1 WHERE id = $1`, card.decision.ID)
		_, err := card.answer(gatedSession, card.signed(issued, "Option A", ""))
		card.wantRefusal(err, http.StatusConflict, "stale_answer")
		if card.consumed(issued) {
			test.Fatal("a stale answer consumed the proof")
		}
	})
	test.Run("policy revision", func(test *testing.T) {
		card := newGatedCard(test, "irreversible")
		issued := card.challenge("Option A", "")
		dbfx.Exec(test, `UPDATE workspace SET policy_revision = policy_revision + 1 WHERE id = $1`, testWorkspaceID)
		_, err := card.answer(gatedSession, card.signed(issued, "Option A", ""))
		card.wantRefusal(err, http.StatusConflict, "stale_answer")
	})
}

func TestGatedAnswerRefusesExpiredSupersededAndConsumedProof(test *testing.T) {
	test.Run("expired", func(test *testing.T) {
		card := newGatedCard(test, "spend")
		issued := card.challenge("Option A", "")
		dbfx.Exec(test, `UPDATE passkey_assertion_challenge SET expires_at = now() - interval '1 second' WHERE id = $1`, issued.ChallengeID)
		_, err := card.answer(gatedSession, card.signed(issued, "Option A", ""))
		card.wantRefusal(err, http.StatusForbidden, "challenge_expired")
	})
	test.Run("superseded", func(test *testing.T) {
		card := newGatedCard(test, "spend")
		first := card.challenge("Option A", "")
		card.challenge("Option A", "")
		_, err := card.answer(gatedSession, card.signed(first, "Option A", ""))
		card.wantRefusal(err, http.StatusForbidden, "challenge_superseded")
	})
	test.Run("consumed", func(test *testing.T) {
		card := newGatedCard(test, "spend")
		issued := card.challenge("Option A", "")
		input := card.signed(issued, "Option A", "")
		if _, err := card.answer(gatedSession, input); err != nil {
			test.Fatal(err)
		}
		withdrawAndReopen(test, card)
		input.AnswerRevision++
		_, err := card.answer(gatedSession, input)
		card.wantRefusal(err, http.StatusForbidden, "assertion_rejected")
	})
}

func withdrawAndReopen(test *testing.T, card *gatedCard) {
	test.Helper()
	dbfx.Exec(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID)
	if _, err := testHandler.Queries.ReopenDecisionRequest(test.Context(), db.ReopenDecisionRequestParams{WorkspaceID: card.issue.WorkspaceID, ID: card.decision.ID}); err != nil {
		test.Fatal(err)
	}
}

func TestReopenAndClassRaiseBumpTheAnswerRevision(test *testing.T) {
	card := newGatedCard(test, "reversible")
	revision := func() int32 {
		var value int32
		dbfx.QueryRow(test, `SELECT answer_revision FROM decision_request WHERE id = $1`, card.decision.ID).Scan(&value)
		return value
	}
	before := revision()
	if _, err := testHandler.Queries.RaiseDecisionClass(test.Context(), db.RaiseDecisionClassParams{WorkspaceID: card.issue.WorkspaceID, ID: card.decision.ID, EscalationClass: "spend"}); err != nil {
		test.Fatal(err)
	}
	if got := revision(); got != before+1 {
		test.Fatalf("after class raise revision = %d, want %d", got, before+1)
	}
	dbfx.Exec(test, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, card.decision.ID)
	if _, err := testHandler.Queries.ReopenDecisionRequest(test.Context(), db.ReopenDecisionRequestParams{WorkspaceID: card.issue.WorkspaceID, ID: card.decision.ID}); err != nil {
		test.Fatal(err)
	}
	if got := revision(); got != before+2 {
		test.Fatalf("after reopen revision = %d, want %d", got, before+2)
	}
}

func TestTwoSimultaneousGatedSubmissionsRecordOneAnswer(test *testing.T) {
	card := newGatedCard(test, "deploy")
	issued := card.challenge("Option A", "")
	input := card.signed(issued, "Option A", "")
	var wait sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, results[i] = card.answer(gatedSession, input)
		}()
	}
	wait.Wait()
	accepted := 0
	for _, err := range results {
		if err == nil {
			accepted++
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`, card.decision.ID); accepted != 1 || count != 1 {
		test.Fatalf("accepted = %d, answers = %d, want 1/1 (%v)", accepted, count, results)
	}
}

func TestChallengeDecisionAnswerRefusals(test *testing.T) {
	card := newGatedCard(test, "irreversible")
	other := dbfx.User(test, "Other", "other-"+uuid.NewString()+"@test.invalid")
	dbfx.Member(test, testWorkspaceID, other, "member")
	for name, tc := range map[string]struct {
		user, source, sid string
		status            int
	}{
		"not interactive": {card.humanID, "pat", gatedSession, http.StatusForbidden},
		"not the human":   {other, "interactive_jwt", gatedSession, http.StatusForbidden},
		"session unbound": {card.humanID, "interactive_jwt", "", http.StatusForbidden},
	} {
		test.Run(name, func(test *testing.T) {
			testutil.Call(test, card.handler.ChallengeDecisionAnswer, card.challengeRequest(tc.user, tc.source, tc.sid, "Option A", "")).Want(tc.status)
		})
	}
	test.Run("not gated", func(test *testing.T) {
		dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'reversible' WHERE id = $1`, card.decision.ID)
		testutil.Call(test, card.handler.ChallengeDecisionAnswer, card.challengeRequest(card.humanID, "interactive_jwt", gatedSession, "Option A", "")).Want(http.StatusConflict)
	})
}
