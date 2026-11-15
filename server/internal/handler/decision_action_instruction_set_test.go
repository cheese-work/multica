package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/decisionaction"
)

// squad.instructions.set and workspace.context.set share one test matrix: the
// same table of cases runs against both actions, as the CHE-3348 plan asks.

const governedText = "rule A <!-- governed:human -->human rule<!-- /governed:human --> <!-- governed:agent -->tail<!-- /governed:agent -->"

type instructionSetTarget struct {
	name, actionType, field, idKey, contentKey string
	// create seeds a target holding governedText and returns its id.
	create func(*testing.T, singleCallFixture) string
	// live reads the stored text; set writes it, the way an ordinary edit would.
	live    func(*testing.T, string) string
	set     func(*testing.T, string, string)
	foreign func(*testing.T, singleCallFixture) string
}

func instructionSetTargets() []instructionSetTarget {
	return []instructionSetTarget{
		{
			name: "squad", actionType: "squad.instructions.set", field: "squad.instructions", idKey: "squad_id", contentKey: "instructions",
			create: func(test *testing.T, fixture singleCallFixture) string {
				id := squadIn(test, fixture, testWorkspaceID)
				dbfx.Exec(test, `UPDATE squad SET instructions = $1 WHERE id = $2`, governedText, id)
				return id
			},
			live: func(test *testing.T, id string) (text string) {
				dbfx.QueryRow(test, `SELECT COALESCE(instructions, '') FROM squad WHERE id = $1`, id).Scan(&text)
				return text
			},
			set: func(test *testing.T, id, text string) {
				dbfx.Exec(test, `UPDATE squad SET instructions = $1 WHERE id = $2`, text, id)
			},
			foreign: func(test *testing.T, fixture singleCallFixture) string {
				return squadIn(test, fixture, foreignWorkspaceID(test))
			},
		},
		{
			name: "workspace", actionType: "workspace.context.set", field: "workspace.context", idKey: "workspace_id", contentKey: "context",
			create: func(test *testing.T, fixture singleCallFixture) string {
				setContextProposerGrant(test, fixture.agentID, true)
				var original string
				dbfx.QueryRow(test, `SELECT COALESCE(context, '') FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original)
				dbfx.Cleanup(test, `UPDATE workspace SET context = $1 WHERE id = $2`, original, testWorkspaceID)
				dbfx.Exec(test, `UPDATE workspace SET context = $1 WHERE id = $2`, governedText, testWorkspaceID)
				return testWorkspaceID
			},
			live: func(test *testing.T, id string) (text string) {
				dbfx.QueryRow(test, `SELECT COALESCE(context, '') FROM workspace WHERE id = $1`, id).Scan(&text)
				return text
			},
			set: func(test *testing.T, id, text string) {
				dbfx.Exec(test, `UPDATE workspace SET context = $1 WHERE id = $2`, text, id)
			},
			foreign: func(test *testing.T, _ singleCallFixture) string { return foreignWorkspaceID(test) },
		},
	}
}

// params is what a correct agent sends for an edit of the governed text.
func (target instructionSetTarget) params(id, before, after string) map[string]any {
	return map[string]any{
		target.idKey: id, target.contentKey: after,
		"base_sha256": decisionaction.TextDigest(before), "after_sha256": decisionaction.TextDigest(after),
	}
}

var editedText = strings.Replace(governedText, "tail", "tail edited", 1)

type instructionSetRequest struct {
	action pendingAction
	target instructionSetTarget
	id     string
}

// requestInstructionSet files the request; edit may break the params first.
func requestInstructionSet(test *testing.T, target instructionSetTarget, status int, edit func(id string, params map[string]any)) (instructionSetRequest, string) {
	test.Helper()
	fixture := newActionFixture(test)
	id := target.create(test, fixture)
	params := target.params(id, governedText, editedText)
	if edit != nil {
		edit(id, params)
	}
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": target.actionType, "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return instructionSetRequest{target: target, id: id}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return instructionSetRequest{pendingAction{fixture, created.ID, created.Action.ID}, target, id}, body
}

func (request instructionSetRequest) preconditions(test *testing.T) map[string]any {
	test.Helper()
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		test.Fatal(err)
	}
	return out
}

func (request instructionSetRequest) claim(test *testing.T) map[string]any {
	test.Helper()
	claim := request.action.claim(test, map[string]string{})
	claim["preview_sha256"] = request.preconditions(test)["preview_sha256"]
	return claim
}

// approveOwner approves as the owner with the preview digest and expects status.
func (request instructionSetRequest) approveOwner(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	makeApproverOwner(test, request.action, "owner")
	return request.action.approve(test, handler, request.claim(test), status)
}

// assertNotApplied proves a refused approval changed nothing.
func (request instructionSetRequest) assertNotApplied(test *testing.T, want string) {
	test.Helper()
	request.action.assertUntouched(test)
	if got := request.target.live(test, request.id); got != want {
		test.Fatalf("stored text %q, want unchanged %q", got, want)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

func forEachInstructionSetTarget(test *testing.T, run func(*testing.T, instructionSetTarget)) {
	test.Helper()
	for _, target := range instructionSetTargets() {
		test.Run(target.name, func(test *testing.T) { run(test, target) })
	}
}

func TestInstructionSetRequestStoresAServerPreview(test *testing.T) {
	forEachInstructionSetTarget(test, func(test *testing.T, target instructionSetTarget) {
		request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
		stored := request.preconditions(test)
		shown, _ := stored["preview"].(map[string]any)
		if shown["field"] != target.field || shown["target_id"] != request.id {
			test.Fatalf("preview %v lacks field %s and target %s", stored["preview"], target.field, request.id)
		}
		if stored["before_sha256"] != decisionaction.TextDigest(governedText) || stored["after_sha256"] != decisionaction.TextDigest(editedText) {
			test.Fatalf("preconditions %v lack the text digests", stored)
		}
		if diff, _ := shown["diff"].([]any); len(diff) == 0 {
			test.Fatalf("preview %v has no diff", shown)
		}
	})
}

func TestInstructionSetRequestRefusals(test *testing.T) {
	forEachInstructionSetTarget(test, func(test *testing.T, target instructionSetTarget) {
		cases := []struct {
			name   string
			status int
			want   string
			edit   func(id string, params map[string]any)
		}{
			{"agent after digest is wrong", http.StatusBadRequest, "after_digest_mismatch", func(_ string, p map[string]any) { p["after_sha256"] = strings.Repeat("0", 64) }},
			{"agent base digest is stale", http.StatusConflict, "precondition_drift", func(_ string, p map[string]any) { p["base_sha256"] = strings.Repeat("0", 64) }},
			{"governed human block edited", http.StatusBadRequest, "governed_blocks_changed", func(id string, p map[string]any) {
				weakened := strings.Replace(governedText, "human rule", "weakened", 1)
				p[target.contentKey], p["after_sha256"] = weakened, decisionaction.TextDigest(weakened)
			}},
			{"another field named in the params", http.StatusBadRequest, "invalid_params", func(_ string, p map[string]any) { p["name"] = "renamed" }},
			{"target of another workspace", http.StatusBadRequest, "target_" + target.name + "_mismatch", nil},
		}
		for _, tc := range cases {
			test.Run(tc.name, func(test *testing.T) {
				edit := tc.edit
				if edit == nil {
					edit = func(_ string, p map[string]any) { p[target.idKey] = target.foreign(test, newActionFixture(test)) }
				}
				_, body := requestInstructionSet(test, target, tc.status, edit)
				mustContain(test, body, tc.want)
			})
		}
	})
}

func TestInstructionSetApproveAppliesAndAudits(test *testing.T) {
	forEachInstructionSetTarget(test, func(test *testing.T, target instructionSetTarget) {
		request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
		// A real enrolled passkey signs the issued challenge: no verifier stub.
		device, keyID := request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		handler := passkeyHandler(test)
		issued := request.action.issue(test, handler)
		claim := request.claim(test)
		claim["assertion"] = request.action.assertion(test, device, issued, passkeyStart+1)
		request.action.approve(test, handler, claim, http.StatusOK)

		if got := target.live(test, request.id); got != editedText {
			test.Fatalf("stored text %q, want %q", got, editedText)
		}
		var raw string
		dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
		var result map[string]any
		if err := json.Unmarshal([]byte(raw), &result); err != nil || result["readback_sha256"] != decisionaction.TextDigest(editedText) {
			test.Fatalf("result %s (%v); want readback_sha256 %s", raw, err, decisionaction.TextDigest(editedText))
		}
		dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`, request.action.issueID).Scan(&raw)
		var details map[string]any
		if err := json.Unmarshal([]byte(raw), &details); err != nil {
			test.Fatal(err)
		}
		for key, want := range map[string]string{
			"field": target.field, "target_id": request.id,
			"before_sha256": decisionaction.TextDigest(governedText), "after_sha256": decisionaction.TextDigest(editedText),
			"decision_request_id": request.action.requestID, "decision_action_id": request.action.actionID, "credential_id": keyID,
		} {
			if details[key] != want {
				test.Fatalf("audit %s = %v, want %s (details %s)", key, details[key], want, raw)
			}
		}
	})
}

func TestInstructionSetApproveRefusals(test *testing.T) {
	forEachInstructionSetTarget(test, func(test *testing.T, target instructionSetTarget) {
		refuse := func(name string, prepare func(request instructionSetRequest), status int, want string) {
			test.Run(name, func(test *testing.T) {
				request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
				before := governedText
				if prepare != nil {
					prepare(request)
					before = target.live(test, request.id)
				}
				mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), status), want)
				request.assertNotApplied(test, before)
			})
		}
		refuse("target drifted after the request", func(request instructionSetRequest) {
			target.set(test, request.id, strings.Replace(governedText, "tail", "tail drifted", 1))
		}, http.StatusConflict, "precondition_drift")
		refuse("governed human block changed after the request", func(request instructionSetRequest) {
			target.set(test, request.id, strings.Replace(governedText, "human rule", "other rule", 1))
		}, http.StatusConflict, "governed_blocks_changed")
		refuse("expired", func(request instructionSetRequest) {
			dbfx.Exec(test, `UPDATE decision_action SET expires_at = now() - interval '1 minute' WHERE id = $1`, request.action.actionID)
		}, http.StatusConflict, "expired")
		refuse("params name another field at approval", func(request instructionSetRequest) {
			dbfx.Exec(test, `UPDATE decision_action SET params = params || '{"name":"renamed"}'::jsonb WHERE id = $1`, request.action.actionID)
		}, http.StatusConflict, "payload_mismatch")
		refuse("params point at a target of another workspace", func(request instructionSetRequest) {
			dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, ARRAY[$1::text], to_jsonb($2::text)) WHERE id = $3`,
				target.idKey, target.foreign(test, request.action.singleCallFixture), request.action.actionID)
		}, http.StatusConflict, "target_"+target.name+"_mismatch")

		test.Run("approver is an admin", func(test *testing.T) {
			request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
			makeApproverOwner(test, request.action, "admin")
			mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
			request.assertNotApplied(test, governedText)
		})
		test.Run("payload digest altered", func(test *testing.T) {
			request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
			makeApproverOwner(test, request.action, "owner")
			claim := request.claim(test)
			claim["payload_sha256"] = strings.Repeat("0", 64)
			mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict), "payload_mismatch")
			request.assertNotApplied(test, governedText)
		})
		test.Run("preview digest absent or altered", func(test *testing.T) {
			request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
			makeApproverOwner(test, request.action, "owner")
			mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.action.claim(test, map[string]string{}), http.StatusConflict), "preview_required")
			claim := request.claim(test)
			claim["preview_sha256"] = strings.Repeat("0", 64)
			request.action.approve(test, withVerifier(acceptingVerifier), claim, http.StatusConflict)
			request.assertNotApplied(test, governedText)
		})
		test.Run("no passkey assertion, a bad one, or one over another challenge", func(test *testing.T) {
			request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
			device, _ := request.action.enrollPasskey(test)
			makeApproverOwner(test, request.action, "owner")
			handler := passkeyHandler(test)
			issued := request.action.issue(test, handler)
			issued.Options.PublicKey.Challenge = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			for _, assertion := range []any{nil, map[string]any{"id": "fake"}, request.action.assertion(test, device, issued, passkeyStart+1)} {
				claim := request.claim(test)
				claim["assertion"] = assertion
				request.action.approve(test, handler, claim, http.StatusForbidden)
			}
			request.assertNotApplied(test, governedText)
		})
		test.Run("owner role revoked between challenge and approve", func(test *testing.T) {
			request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
			device, _ := request.action.enrollPasskey(test)
			makeApproverOwner(test, request.action, "owner")
			handler := passkeyHandler(test)
			issued := request.action.issue(test, handler)
			makeApproverOwner(test, request.action, "admin")
			claim := request.claim(test)
			claim["assertion"] = request.action.assertion(test, device, issued, passkeyStart+1)
			mustContain(test, request.action.approve(test, handler, claim, http.StatusForbidden), "class_needs_owner")
			request.assertNotApplied(test, governedText)
		})
		test.Run("second approve of the same action", func(test *testing.T) {
			request, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
			handler := withVerifier(acceptingVerifier)
			request.approveOwner(test, handler, http.StatusOK)
			request.action.approve(test, handler, request.claim(test), http.StatusConflict)
			if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
				test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
			}
		})
	})
}

func TestInstructionSetConcurrentApprovalsApplyOnce(test *testing.T) {
	forEachInstructionSetTarget(test, func(test *testing.T, target instructionSetTarget) {
		first, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
		// The second agent saw the same base and wants a different edit.
		other := strings.Replace(governedText, "tail", "tail other", 1)
		fixture := newActionFixture(test)
		if target.name == "workspace" {
			setContextProposerGrant(test, fixture.agentID, true)
		}
		var created actionResponse
		fixture.respond(test, fixture.withAction(map[string]any{"type": target.actionType, "version": 1, "params": target.params(first.id, governedText, other)}), http.StatusCreated, &created)
		dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
		second := instructionSetRequest{pendingAction{fixture, created.ID, created.Action.ID}, target, first.id}

		makeApproverOwner(test, first.action, "owner")
		handler := withVerifier(acceptingVerifier)
		statuses, bodies := make([]int, 2), make([]string, 2)
		var group sync.WaitGroup
		for i, request := range []instructionSetRequest{first, second} {
			group.Add(1)
			go func() {
				defer group.Done()
				raw, err := json.Marshal(request.claim(test))
				if err != nil {
					test.Error(err)
					return
				}
				call := request.action.decide("approve", request.action.humanID, "interactive_jwt")
				call.Body = readerCloser{bytes.NewReader(raw)}
				recorder := httptest.NewRecorder()
				handler.ApproveDecisionAction(recorder, call)
				statuses[i], bodies[i] = recorder.Code, recorder.Body.String()
			}()
		}
		group.Wait()
		applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
		if applied != 1 {
			test.Fatalf("applied %d times (statuses %v), want exactly 1", applied, statuses)
		}
		// The loser is refused with 409: precondition_drift when the winner had
		// already committed, busy_retry (the core's contract, see
		// TestConfigChangeBusyTargetAnswersBusyRetry) when the winner still held
		// the target lock. Either way it changed nothing.
		winners := 0
		for i, status := range statuses {
			switch status {
			case http.StatusOK:
				winners++
			case http.StatusConflict:
				if !strings.Contains(bodies[i], "precondition_drift") && !strings.Contains(bodies[i], "busy_retry") {
					test.Fatalf("loser body %s, want precondition_drift or busy_retry", bodies[i])
				}
			default:
				test.Fatalf("approval %d answered %d (%s)", i, status, bodies[i])
			}
		}
		if winners != 1 {
			test.Fatalf("statuses %v, want one 200 and one 409", statuses)
		}
		if got := target.live(test, first.id); got != editedText && got != other {
			test.Fatalf("stored text %q is neither approved edit", got)
		}
	})
}

// The signed matrix row: a loser that got busy_retry gets precondition_drift
// on one retry after the winning approval commits, and never mutates. The test
// holds the field lock to force busy_retry, so the sequence is deterministic.
func TestInstructionSetBusyLoserRetriesAsDriftAfterTheWinnerCommits(test *testing.T) {
	forEachInstructionSetTarget(test, func(test *testing.T, target instructionSetTarget) {
		winner, _ := requestInstructionSet(test, target, http.StatusCreated, nil)
		other := strings.Replace(governedText, "tail", "tail other", 1)
		fixture := newActionFixture(test)
		if target.name == "workspace" {
			setContextProposerGrant(test, fixture.agentID, true)
		}
		var created actionResponse
		fixture.respond(test, fixture.withAction(map[string]any{"type": target.actionType, "version": 1, "params": target.params(winner.id, governedText, other)}), http.StatusCreated, &created)
		dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
		loser := instructionSetRequest{pendingAction{fixture, created.ID, created.Action.ID}, target, winner.id}
		handler := withVerifier(acceptingVerifier)

		release := holdRowLock(test, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, configChangeLockKey(testWorkspaceID, target.field, winner.id))
		mustContain(test, loser.approveOwner(test, handler, http.StatusConflict), "busy_retry")
		loser.assertNotApplied(test, governedText)

		release()
		winner.approveOwner(test, handler, http.StatusOK)
		mustContain(test, loser.approveOwner(test, handler, http.StatusConflict), "precondition_drift")
		loser.action.assertUntouched(test)
		if got := target.live(test, winner.id); got != editedText {
			test.Fatalf("stored text %q after the losing retry, want the winner's %q", got, editedText)
		}
		if n := loser.action.activityCount(test, "config_change_applied"); n != 0 {
			test.Fatalf("losing action wrote %d config_change_applied rows, want 0", n)
		}
		if n := winner.action.activityCount(test, "config_change_applied"); n != 1 {
			test.Fatalf("winning action wrote %d config_change_applied rows, want 1", n)
		}
	})
}

func TestSquadInstructionsSetRefusesGrowthPastTheTokenLimit(test *testing.T) {
	target := instructionSetTargets()[0]
	long := governedText + strings.Repeat(" word", 4000)
	_, body := requestInstructionSet(test, target, http.StatusUnprocessableEntity, func(_ string, p map[string]any) {
		p[target.contentKey], p["after_sha256"] = long, decisionaction.TextDigest(long)
	})
	mustContain(test, body, "exceed")
}
