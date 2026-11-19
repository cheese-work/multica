package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5682 layer 2: the decided-ask guard. Each case sends one ask with the
// guard off, records the person's card answer, switches the mode and sends a
// second ask on the same issue chain.

const decidedModeKey = "ask_guard_decided_mode"

func setDecidedMode(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{`+decidedModeKey+`}', to_jsonb($1::text)) WHERE id = $2`, mode, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - '`+decidedModeKey+`' WHERE id = $1`, testWorkspaceID)
	})
}

func decidedEvents(t *testing.T, issueID, mode string) int {
	t.Helper()
	return askGuardEventCount(t, `operation_kind = 'decided_ask' AND issue_id = $1 AND mode = $2`, issueID, mode)
}

// send posts ask as the fixture's agent; mutate edits the request body.
func (f singleCallFixture) send(t *testing.T, ask decisiontx.Ask, status int, mutate func(map[string]any)) map[string]any {
	t.Helper()
	body := f.body()
	ask.SourceIssueID, ask.SourceCommentID = f.issueID, f.sourceID
	body["ask"] = ask
	if mutate != nil {
		mutate(body)
	}
	var out map[string]any
	testutil.Call(t, testHandler.RequestDecision, f.call(body)).Want(status).JSON(&out)
	return out
}

// seed sends ask (guard off) and records a card answer that chose option 0.
func (f singleCallFixture) seed(t *testing.T, ask decisiontx.Ask, at time.Time) (requestID string) {
	t.Helper()
	requestID = f.send(t, ask, http.StatusCreated, nil)["id"].(string)
	answer(t, requestID, f.humanID, 0, at)
	return requestID
}

func answer(t *testing.T, requestID, humanID string, chosen int, at time.Time) string {
	t.Helper()
	dbfx.Exec(t, `UPDATE decision_request SET status = 'answered' WHERE id = $1`, requestID)
	return dbfx.Insert(t, "decision_answer", testutil.Cols{
		"decision_request_id": requestID, "answered_by_user_id": humanID, "answer_source": "card",
		"chosen_option_index": chosen, "chat_message_ids": []string{}, "content_snapshot": "Proceed", "effective_at": at,
	})
}

func (f singleCallFixture) agentAsk(mutate func(*decisiontx.Ask)) decisiontx.Ask {
	ask := f.details
	ask.Options = append([]decisiontx.AskOption(nil), ask.Options...)
	if mutate != nil {
		mutate(&ask)
	}
	return ask
}

func wantBlocked(t *testing.T, body map[string]any, requestID string) {
	t.Helper()
	if body["code"] != "decision_already_answered" || body["decision_request_id"] != requestID {
		t.Fatalf("want 409 decision_already_answered for %s, got %v", requestID, body)
	}
}

// otherAgent is a second agent with its own running task on the same issue.
func (f singleCallFixture) otherAgent(t *testing.T) singleCallFixture {
	t.Helper()
	other := f
	other.agentName = "decided-other-" + uuid.NewString()[:8]
	other.agentID = createHandlerTestAgent(t, other.agentName, []byte("[]"))
	other.taskID = dbfx.Task(t, other.agentID, testutil.Cols{"issue_id": f.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
	other.details.AskedBy = other.agentName
	return other
}

// Test 1: match, per mode. Test 15: a different agent, nothing else changed.
func TestDecidedAskMatchByMode(t *testing.T) {
	for _, mode := range []string{"off", "warn", "block"} {
		t.Run(mode, func(t *testing.T) {
			f := newSingleCallFixture(t)
			requestID := f.seed(t, f.details, time.Now().Add(-time.Hour))
			setDecidedMode(t, mode)
			other := f.otherAgent(t)
			if mode != "block" {
				other.send(t, other.details, http.StatusCreated, nil)
				want := 0
				if mode == "warn" {
					want = 1
				}
				if n := decidedEvents(t, f.issueID, mode); n != want {
					t.Fatalf("%s events = %d, want %d", mode, n, want)
				}
				return
			}
			before, _, _ := f.counts(t)
			body := other.send(t, other.details, http.StatusConflict, nil)
			wantBlocked(t, body, requestID)
			for _, field := range []string{"answer_id", "chosen_option", "answered_by", "answered_at", "issue", "record_link"} {
				if s, _ := body[field].(string); s == "" {
					t.Fatalf("409 lacks %s: %v", field, body)
				}
			}
			if after, _, _ := f.counts(t); after != before {
				t.Fatalf("blocked ask wrote a comment: %d -> %d", before, after)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM decision_request WHERE issue_id = $1`, f.issueID); n != 1 {
				t.Fatalf("blocked ask wrote a request: %d rows", n)
			}
			if n := decidedEvents(t, f.issueID, "block"); n != 1 {
				t.Fatalf("block events = %d, want 1 committed before the 409", n)
			}
		})
	}
}

// Tests 2-4 and wiring for 11: an ask that differs is never blocked.
func TestDecidedAskDifferentAskIsAllowed(t *testing.T) {
	cases := map[string]func(*decisiontx.Ask){
		"decision text":  func(a *decisiontx.Ask) { a.Decision = "Ship the other change?" },
		"label added":    func(a *decisiontx.Ask) { a.Options = append(a.Options, testAskOption("Defer")) },
		"label renamed":  func(a *decisiontx.Ask) { a.Options[0].Label = "Go" },
		"option content": func(a *decisiontx.Ask) { a.Options[1].WhatHappens = "Something else happens." },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSingleCallFixture(t)
			f.seed(t, f.details, time.Now().Add(-time.Hour))
			setDecidedMode(t, "block")
			f.send(t, f.agentAsk(change), http.StatusCreated, nil)
			if n := decidedEvents(t, f.issueID, "block"); n != 0 {
				t.Fatalf("%s wrote %d events", name, n)
			}
		})
	}
}

func TestDecidedAskSameOptionsInAnotherOrderStillMatch(t *testing.T) {
	f := newSingleCallFixture(t)
	requestID := f.seed(t, f.details, time.Now().Add(-time.Hour))
	setDecidedMode(t, "block")
	ask := f.agentAsk(func(a *decisiontx.Ask) { a.Options[0], a.Options[1] = a.Options[1], a.Options[0] })
	wantBlocked(t, f.otherAgent(t).send(t, ask, http.StatusConflict, nil), requestID)
}

func TestDecidedAskOtherRecipientAndScopeAreAllowed(t *testing.T) {
	t.Run("recipient", func(t *testing.T) {
		f := newSingleCallFixture(t)
		f.seed(t, f.details, time.Now().Add(-time.Hour))
		second := dbfx.User(t, "Second human", "second-"+uuid.NewString()+"@test.invalid")
		dbfx.Insert(t, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": second, "role": "owner"})
		setDecidedMode(t, "block")
		f.send(t, f.details, http.StatusCreated, func(body map[string]any) { body["for"] = second })
	})
	t.Run("escalation class", func(t *testing.T) {
		f := newSingleCallFixture(t)
		f.seed(t, f.details, time.Now().Add(-time.Hour))
		setDecidedMode(t, "block")
		f.send(t, f.details, http.StatusCreated, func(body map[string]any) { body["escalation_class"] = "product_direction" })
	})
	t.Run("sibling issue", func(t *testing.T) {
		f, sibling := newSingleCallFixture(t), newSingleCallFixture(t)
		f.seed(t, f.details, time.Now().Add(-time.Hour))
		setDecidedMode(t, "block")
		sibling.humanID = f.humanID
		sibling.send(t, sibling.details, http.StatusCreated, nil)
	})
}

// Test 5: a disclaimed or withdrawn answer no longer stands.
func TestDecidedAskDisclaimedOrWithdrawnAnswerIsAllowed(t *testing.T) {
	for name, undo := range map[string]string{
		"disclaimed": `UPDATE decision_answer SET disclaimed_at = now(), disclaimed_by_user_id = answered_by_user_id, disclaim_reason = 'human' WHERE decision_request_id = $1`,
		"withdrawn":  `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'agent_withdrawn' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newSingleCallFixture(t)
			requestID := f.seed(t, f.details, time.Now().Add(-time.Hour))
			dbfx.Exec(t, undo, requestID)
			setDecidedMode(t, "block")
			f.otherAgent(t).send(t, f.details, http.StatusCreated, nil)
		})
	}
}

// Test 6: only the latest effective decision counts.
func TestDecidedAskOlderSupersededAnswerNeverMatches(t *testing.T) {
	f := newSingleCallFixture(t)
	x := f.details
	y := f.agentAsk(func(a *decisiontx.Ask) { a.Options = append(a.Options, testAskOption("Defer")) })
	f.seed(t, x, time.Now().Add(-2*time.Hour))
	yID := f.seed(t, y, time.Now().Add(-time.Hour))
	setDecidedMode(t, "block")
	other := f.otherAgent(t)
	wantBlocked(t, other.send(t, y, http.StatusConflict, nil), yID)
	other.send(t, x, http.StatusCreated, nil)
}

// Test 7: supersession across the parent chain.
func TestDecidedAskChainSupersession(t *testing.T) {
	parent, child, grandchild := newSingleCallFixture(t), newSingleCallFixture(t), newSingleCallFixture(t)
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = $1 WHERE id = $2`, parent.issueID, child.issueID)
	dbfx.Exec(t, `UPDATE issue SET parent_issue_id = $1 WHERE id = $2`, child.issueID, grandchild.issueID)
	child.humanID, grandchild.humanID = parent.humanID, parent.humanID
	x := parent.details
	y := parent.agentAsk(func(a *decisiontx.Ask) { a.Options = append(a.Options, testAskOption("Defer")) })
	parent.seed(t, x, time.Now().Add(-2*time.Hour))
	yID := child.seed(t, y, time.Now().Add(-time.Hour))
	setDecidedMode(t, "block")
	wantBlocked(t, grandchild.send(t, y, http.StatusConflict, nil), yID)
	grandchild.send(t, x, http.StatusCreated, nil)
}

// Test 8: ambiguous or incomparable evidence never blocks.
func TestDecidedAskAmbiguousEvidenceIsAllowed(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	cases := map[string]func(t *testing.T, f singleCallFixture, requestID string){
		"tie on effective_at with another choice": func(t *testing.T, f singleCallFixture, _ string) {
			other := f.agentAsk(func(a *decisiontx.Ask) { a.Options = append(a.Options, testAskOption("Defer")) })
			id := f.send(t, other, http.StatusCreated, nil)["id"].(string)
			answer(t, id, f.humanID, 1, at)
		},
		"newer untyped record": func(t *testing.T, f singleCallFixture, requestID string) {
			var sessionID string
			dbfx.QueryRow(t, `SELECT chat_session_id::text FROM decision_request WHERE id = $1`, requestID).Scan(&sessionID)
			id := dbfx.Insert(t, "decision_request", testutil.Cols{
				"workspace_id": testWorkspaceID, "issue_id": f.issueID, "asking_agent_id": f.agentID, "human_user_id": f.humanID,
				"ask": "A free-form decision.", "escalation_class": "reversible", "status": "answered", "origin": "unprompted",
				"authority": "human", "chat_session_id": sessionID, "request_fingerprint": "untyped-" + uuid.NewString(),
			})
			answer(t, id, f.humanID, 0, at.Add(time.Minute))
		},
		"free-text answer": func(t *testing.T, _ singleCallFixture, requestID string) {
			dbfx.Exec(t, `UPDATE decision_answer SET chosen_option_index = NULL WHERE decision_request_id = $1`, requestID)
		},
		"agent-recorded answer": func(t *testing.T, f singleCallFixture, requestID string) {
			dbfx.Exec(t, `UPDATE decision_answer SET answer_source = 'agent_record', answered_by_user_id = NULL, answered_by_task_id = $2, source_comment_id = $3 WHERE decision_request_id = $1`, requestID, f.taskID, f.sourceID)
		},
		"legacy ask without ask_json": func(t *testing.T, _ singleCallFixture, requestID string) {
			dbfx.Exec(t, `UPDATE decision_request SET ask_json = NULL WHERE id = $1`, requestID)
		},
		"reopened request": func(t *testing.T, _ singleCallFixture, requestID string) {
			dbfx.Exec(t, `UPDATE decision_request SET status = 'open' WHERE id = $1`, requestID)
		},
		"unknown field in stored ask_json": func(t *testing.T, _ singleCallFixture, requestID string) {
			dbfx.Exec(t, `UPDATE decision_request SET ask_json = ask_json || '{"retired_field": "x"}'::jsonb WHERE id = $1`, requestID)
		},
		"proposal row (paths never cross)": func(t *testing.T, _ singleCallFixture, requestID string) {
			dbfx.Exec(t, `UPDATE decision_request SET proposal_fingerprint = 'p-' || id::text WHERE id = $1`, requestID)
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSingleCallFixture(t)
			requestID := f.seed(t, f.details, at)
			spoil(t, f, requestID)
			setDecidedMode(t, "block")
			f.otherAgent(t).send(t, f.details, http.StatusCreated, nil)
			if n := decidedEvents(t, f.issueID, "block"); n != 0 {
				t.Fatalf("%s wrote %d block events", name, n)
			}
		})
	}
}

// Test 9: the ask path and the two-step intent path.
func TestDecidedAskAskPathBlocks(t *testing.T) {
	f := newAskFixture(t)
	requestID := f.seed(t, f.details, time.Now().Add(-time.Hour))
	setDecidedMode(t, "block")
	other := f.otherAgent(t)
	var body map[string]any
	testutil.Call(t, testHandler.AskDecision, other.askCall(other.askBody())).Want(http.StatusConflict).JSON(&body)
	wantBlocked(t, body, requestID)
}

func TestDecidedAskTwoStepIntentPathBlocks(t *testing.T) {
	fixture := newRequestCoreFixture(t, false)
	testutil.Call(t, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusCreated)
	var requestID string
	dbfx.QueryRow(t, `SELECT id::text FROM decision_request WHERE issue_id = $1`, fixture.issueID).Scan(&requestID)
	answer(t, requestID, fixture.humanID, 0, time.Now().Add(-time.Hour))
	dbfx.Exec(t, `UPDATE decision_request SET request_fingerprint = request_fingerprint || '-sent' WHERE id = $1`, requestID) // no idempotent replay
	setDecidedMode(t, "block")
	openLaneIntent(t, fixture.intentFixture, fixture.mention())
	var body map[string]any
	testutil.Call(t, testHandler.RequestDecision, fixture.apiRequest()).Want(http.StatusConflict).JSON(&body)
	wantBlocked(t, body, requestID)
}

// Test 10: the existing guard is unchanged and independent.
func TestDecidedAskIsIndependentOfAskGuardMode(t *testing.T) {
	f := newSingleCallFixture(t)
	f.seed(t, f.details, time.Now().Add(-time.Hour))
	setAskGuardMode(t, "block")
	setDecidedMode(t, "warn")
	f.otherAgent(t).send(t, f.details, http.StatusCreated, nil)
	if n := decidedEvents(t, f.issueID, "warn"); n != 1 {
		t.Fatalf("warn events = %d, want 1", n)
	}
}

func TestDecidedAskSettingIsValidated(t *testing.T) {
	if err := validateAskGuardSetting(map[string]any{decidedModeKey: "loud"}); err == nil {
		t.Fatal("a bad mode must be refused")
	}
	if err := validateAskGuardSetting(map[string]any{decidedModeKey: "block"}); err != nil {
		t.Fatal(err)
	}
}
