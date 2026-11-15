package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The test action edits step instructions of a workflow through the shared
// config-change core. It stands in for the concrete actions of later steps.
const configAction = "test.config.workflow"

const governedStep = "keep <!-- governed:human -->human rule<!-- /governed:human --> tail"

var (
	// configField is the field the test spec reports; a test flips it after the
	// request to prove the denylist is read again at approval.
	configField atomic.Value
	// configAfterSuffix changes what the spec computes after the request, to
	// prove the server recomputes the agent's after digest at approval.
	configAfterSuffix atomic.Value
	configPublished   atomic.Int32
	// configBeforeApply runs inside the approval just before Apply writes, where
	// a competing ordinary edit would land if the target row were not locked.
	configBeforeApply atomic.Pointer[func()]
	// configBeforeLock runs inside the approval before the action's own row lock.
	configBeforeLock atomic.Pointer[func()]
	// configAuthorize is the test action's Authorize hook body; it records each
	// proposer it sees and may refuse.
	configAuthorize atomic.Pointer[func(proposer pgtype.UUID) error]
	configProposers struct {
		sync.Mutex
		seen []string
	}
)

func seenProposers() []string {
	configProposers.Lock()
	defer configProposers.Unlock()
	return append([]string(nil), configProposers.seen...)
}

func registerConfigAction(test *testing.T) {
	test.Helper()
	configField.Store("workflow.instructions")
	configAfterSuffix.Store("")
	configPublished.Store(0)
	configBeforeApply.Store(nil)
	configBeforeLock.Store(nil)
	configAuthorize.Store(nil)
	configProposers.Lock()
	configProposers.seen = nil
	configProposers.Unlock()
	decisionaction.Register(&decisionaction.Definition{
		Type: configAction, Version: 1, MinClass: "spend", Target: decisionaction.TargetWorkflow, RequiresPreview: true,
		Normalize: func(raw json.RawMessage, slots []decisionaction.SecretSlot) (json.RawMessage, []decisionaction.SecretSlot, error) {
			return raw, slots, nil
		},
	})
	registerConfigChange(configAction, configChange{
		Fields: []string{"workflow.instructions"},
		Authorize: func(_ context.Context, _ *db.Queries, _, proposer pgtype.UUID, _ json.RawMessage, _ configChangeRead) error {
			configProposers.Lock()
			configProposers.seen = append(configProposers.seen, uuidToString(proposer))
			configProposers.Unlock()
			if hook := configAuthorize.Load(); hook != nil {
				return (*hook)(proposer)
			}
			return nil
		},
		Lock: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) error {
			if hook := configBeforeLock.Load(); hook != nil {
				(*hook)()
			}
			workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, http.StatusConflict, false)
			if err != nil {
				return err
			}
			_, err = queries.LockWorkflowTargetNowait(ctx, db.LockWorkflowTargetNowaitParams{ID: workflow.ID, WorkspaceID: workspaceID})
			return err
		},
		Read: func(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, params json.RawMessage) (configChangeRead, error) {
			workflow, err := loadWorkflowTarget(ctx, queries, workspaceID, params, http.StatusBadRequest, false)
			if err != nil {
				return configChangeRead{}, err
			}
			var wanted struct {
				SetInstructions map[string]string `json:"set_instructions"`
			}
			if err := json.Unmarshal(params, &wanted); err != nil {
				return configChangeRead{}, err
			}
			live, err := issueworkflow.DecodeSteps(workflow.Steps)
			if err != nil {
				return configChangeRead{}, err
			}
			after, err := configEditedSteps(live, wanted.SetInstructions, configAfterSuffix.Load().(string))
			if err != nil {
				return configChangeRead{}, err
			}
			return configChangeRead{
				Field: configField.Load().(string), TargetID: uuidToString(workflow.ID),
				Before: stepInstructions(live), After: stepInstructions(after),
				LiveSHA256: workflowStepsHash(workflow.ID, live), AfterSHA256: workflowStepsHash(workflow.ID, after),
				HumanBlocksIntact: protectedStepBlocksUnchanged(live, after),
			}, nil
		},
		Apply: func(ctx context.Context, execution decisionActionExecution, read configChangeRead) error {
			if hook := configBeforeApply.Load(); hook != nil {
				(*hook)()
			}
			var wanted struct {
				SetInstructions map[string]string `json:"set_instructions"`
			}
			if err := json.Unmarshal(execution.Action.Params, &wanted); err != nil {
				return err
			}
			workflow, err := loadWorkflowTarget(ctx, execution.Queries, execution.Action.WorkspaceID, execution.Action.Params, http.StatusConflict, false)
			if err != nil {
				return err
			}
			live, err := issueworkflow.DecodeSteps(workflow.Steps)
			if err != nil {
				return err
			}
			after, err := configEditedSteps(live, wanted.SetInstructions, configAfterSuffix.Load().(string))
			if err != nil {
				return err
			}
			encoded, err := issueworkflow.EncodeSteps(after)
			if err != nil {
				return err
			}
			_, err = execution.Queries.UpdateIssueWorkflow(ctx, db.UpdateIssueWorkflowParams{
				Name: workflow.Name, Description: workflow.Description, InitialStatusKey: workflow.InitialStatusKey,
				Steps: encoded, MaxRunsPerIssue: workflow.MaxRunsPerIssue, IncludeChildPrs: pgtype.Bool{Bool: workflow.IncludeChildPrs, Valid: true},
				ID: workflow.ID, WorkspaceID: workflow.WorkspaceID,
			})
			return err
		},
		Publish: func(context.Context, *Handler, db.DecisionAction) error {
			configPublished.Add(1)
			return nil
		},
	})
	test.Cleanup(func() {
		delete(decisionActionExecutors, configAction)
		delete(configChanges, configAction)
	})
}

// configEditedSteps applies the instruction edits and normalizes through the stored
// encoding, the way the live row will read back.
func configEditedSteps(live []issueworkflow.Step, set map[string]string, suffix string) ([]issueworkflow.Step, error) {
	edited := append([]issueworkflow.Step(nil), live...)
	for i, step := range edited {
		if text, ok := set[step.StatusKey]; ok {
			edited[i].Instructions = text + suffix
		}
	}
	encoded, err := issueworkflow.EncodeSteps(edited)
	if err != nil {
		return nil, err
	}
	return issueworkflow.DecodeSteps(encoded)
}

func stepInstructions(steps []issueworkflow.Step) map[string]string {
	out := make(map[string]string, len(steps))
	for _, step := range steps {
		out[step.StatusKey] = step.Instructions
	}
	return out
}

// configWorkflow inserts a two-step workflow whose second step carries a
// governed:human block, and returns its id and live steps digest.
func configWorkflow(test *testing.T) (id, digest string) {
	test.Helper()
	member := issueworkflow.Handler{Type: issueworkflow.HandlerMember, ID: testUserID}
	encoded, err := issueworkflow.EncodeSteps([]issueworkflow.Step{
		{StatusKey: "todo", Handler: member, Instructions: "do A", NextStatusKey: "in_review"},
		{StatusKey: "in_review", Handler: member, Instructions: governedStep},
	})
	if err != nil {
		test.Fatal(err)
	}
	dbfx.QueryRow(test, `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps) VALUES ($1, 'config wf', 'todo', $2::jsonb) RETURNING id`, testWorkspaceID, string(encoded)).Scan(&id)
	dbfx.Cleanup(test, `DELETE FROM issue_workflow WHERE id = $1`, id)
	return id, liveStepsDigest(test, id)
}

func liveStepsDigest(test *testing.T, workflowID string) string {
	test.Helper()
	var raw []byte
	dbfx.QueryRow(test, `SELECT steps FROM issue_workflow WHERE id = $1`, workflowID).Scan(&raw)
	steps, err := issueworkflow.DecodeSteps(raw)
	if err != nil {
		test.Fatal(err)
	}
	return workflowStepsHash(parseUUID(workflowID), steps)
}

// afterDigest is the digest a correct agent computes for its own edit.
func afterDigest(test *testing.T, workflowID string, set map[string]string) string {
	test.Helper()
	var raw []byte
	dbfx.QueryRow(test, `SELECT steps FROM issue_workflow WHERE id = $1`, workflowID).Scan(&raw)
	live, err := issueworkflow.DecodeSteps(raw)
	if err != nil {
		test.Fatal(err)
	}
	after, err := configEditedSteps(live, set, "")
	if err != nil {
		test.Fatal(err)
	}
	return workflowStepsHash(parseUUID(workflowID), after)
}

type configRequest struct {
	action     pendingAction
	workflowID string
	base       string
	set        map[string]string
}

// requestConfig asks for a one-step edit; edit may break the params first.
func requestConfig(test *testing.T, status int, edit func(workflowID string, params map[string]any)) (configRequest, string) {
	test.Helper()
	registerConfigAction(test)
	workflowID, base := configWorkflow(test)
	set := map[string]string{"todo": "do A better"}
	params := map[string]any{"workflow_id": workflowID, "set_instructions": set, "base_sha256": base, "after_sha256": afterDigest(test, workflowID, set)}
	if edit != nil {
		edit(workflowID, params)
	}
	fixture := newActionFixture(test)
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": configAction, "version": 1, "params": params}), status, &created)
	if status != http.StatusCreated {
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions; want none", requests, actions)
		}
		return configRequest{workflowID: workflowID, base: base}, body
	}
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return configRequest{pendingAction{fixture, created.ID, created.Action.ID}, workflowID, base, set}, body
}

func (request configRequest) preconditions(test *testing.T) map[string]any {
	test.Helper()
	var raw string
	dbfx.QueryRow(test, `SELECT preconditions::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		test.Fatal(err)
	}
	return out
}

func (request configRequest) claim(test *testing.T) map[string]any {
	test.Helper()
	claim := request.action.claim(test, map[string]string{})
	claim["preview_sha256"] = request.preconditions(test)["preview_sha256"]
	return claim
}

// approveOwner approves as the owner with the preview digest and expects status.
func (request configRequest) approveOwner(test *testing.T, handler *Handler, status int) string {
	test.Helper()
	makeApproverOwner(test, request.action, "owner")
	return request.action.approve(test, handler, request.claim(test), status)
}

// assertNotApplied proves a refused approval changed nothing.
func (request configRequest) assertNotApplied(test *testing.T, want string) {
	test.Helper()
	request.action.assertUntouched(test)
	if got := liveStepsDigest(test, request.workflowID); got != want {
		test.Fatalf("workflow digest %s, want unchanged %s", got, want)
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 0 {
		test.Fatalf("config_change_applied rows = %d, want 0", n)
	}
}

func TestConfigChangeRequestStoresAServerPreview(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	stored := request.preconditions(test)
	preview, _ := json.Marshal(stored["preview"])
	canonical, err := decisionaction.Canonical(preview)
	if err != nil {
		test.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	if stored["preview_sha256"] != hex.EncodeToString(sum[:]) {
		test.Fatalf("preview_sha256 %v is not the digest of the stored preview %s", stored["preview_sha256"], canonical)
	}
	shown := stored["preview"].(map[string]any)
	if shown["field"] != "workflow.instructions" || shown["target_id"] != request.workflowID {
		test.Fatalf("preview %s lacks field and target", canonical)
	}
	if shown["before_sha256"] != request.base || shown["after_sha256"] != afterDigest(test, request.workflowID, request.set) {
		test.Fatalf("preview %s lacks the before and after digests", canonical)
	}
	if diff, _ := shown["diff"].([]any); len(diff) == 0 {
		test.Fatalf("preview %s has no diff", canonical)
	}
	if stored["before_sha256"] != request.base || stored["after_sha256"] != shown["after_sha256"] {
		test.Fatalf("preconditions %v lack the digests", stored)
	}
}

func TestConfigChangeRequestRefusals(test *testing.T) {
	test.Run("agent after digest is wrong", func(test *testing.T) {
		_, body := requestConfig(test, http.StatusBadRequest, func(_ string, params map[string]any) { params["after_sha256"] = strings.Repeat("0", 64) })
		mustContain(test, body, "after_digest_mismatch")
	})
	test.Run("agent base digest is stale", func(test *testing.T) {
		_, body := requestConfig(test, http.StatusConflict, func(_ string, params map[string]any) { params["base_sha256"] = strings.Repeat("0", 64) })
		mustContain(test, body, "precondition_drift")
	})
	test.Run("field outside the allow list", func(test *testing.T) {
		registerConfigAction(test)
		configField.Store("member.role")
		workflowID, base := configWorkflow(test)
		set := map[string]string{"todo": "x"}
		fixture := newActionFixture(test)
		params := map[string]any{"workflow_id": workflowID, "set_instructions": set, "base_sha256": base, "after_sha256": afterDigest(test, workflowID, set)}
		body := fixture.respond(test, fixture.withAction(map[string]any{"type": configAction, "version": 1, "params": params}), http.StatusForbidden, nil)
		mustContain(test, body, "denylisted")
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("denied request stored %d requests and %d actions", requests, actions)
		}
	})
	test.Run("governed human block edited", func(test *testing.T) {
		_, body := requestConfig(test, http.StatusBadRequest, func(workflowID string, params map[string]any) {
			set := map[string]string{"in_review": "keep <!-- governed:human -->weakened<!-- /governed:human --> tail"}
			params["set_instructions"], params["after_sha256"] = set, afterDigest(test, workflowID, set)
		})
		mustContain(test, body, "governed_blocks_changed")
	})
	test.Run("workflow of another workspace", func(test *testing.T) {
		_, body := requestConfig(test, http.StatusBadRequest, func(_ string, params map[string]any) { params["workflow_id"] = foreignWorkspaceWorkflow(test) })
		mustContain(test, body, "target_workflow_mismatch")
	})
}

func TestConfigChangeApproveAppliesAuditsAndPublishes(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	_, keyID := request.action.enrollPasskey(test)
	verify := func(context.Context, *db.Queries, ApprovalClaim) (pgtype.UUID, error) { return parseUUID(keyID), nil }
	wantAfter := afterDigest(test, request.workflowID, request.set)
	request.approveOwner(test, withVerifier(verify), http.StatusOK)

	if got := liveStepsDigest(test, request.workflowID); got != wantAfter {
		test.Fatalf("workflow digest %s, want %s", got, wantAfter)
	}
	var result map[string]any
	var raw string
	dbfx.QueryRow(test, `SELECT result::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&raw)
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result["readback_sha256"] != wantAfter {
		test.Fatalf("result %s (%v); want readback_sha256 %s", raw, err, wantAfter)
	}
	var details map[string]any
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'config_change_applied'`, request.action.issueID).Scan(&raw)
	if err := json.Unmarshal([]byte(raw), &details); err != nil {
		test.Fatal(err)
	}
	for key, want := range map[string]string{
		"field": "workflow.instructions", "target_id": request.workflowID, "before_sha256": request.base, "after_sha256": wantAfter,
		"decision_request_id": request.action.requestID, "decision_action_id": request.action.actionID, "credential_id": keyID,
		"agent_id": request.action.agentID, "task_id": request.action.taskID,
	} {
		if details[key] != want {
			test.Fatalf("audit %s = %v, want %s (details %s)", key, details[key], want, raw)
		}
	}
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d, want 1", n)
	}
	if got := configPublished.Load(); got != 1 {
		test.Fatalf("publish hook ran %d times, want 1", got)
	}
}

func TestConfigChangeApproveRefusals(test *testing.T) {
	test.Run("target drifted after the request", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		dbfx.Exec(test, `UPDATE issue_workflow SET description = description, steps = replace(steps::text, 'do A', 'do A2')::jsonb WHERE id = $1`, request.workflowID)
		drifted := liveStepsDigest(test, request.workflowID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "precondition_drift")
		request.assertNotApplied(test, drifted)
	})
	test.Run("after digest no longer recomputes", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		configAfterSuffix.Store(" changed")
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "after_digest_mismatch")
		request.assertNotApplied(test, request.base)
	})
	test.Run("field denied at approval", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		configField.Store("passkey.credential")
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusForbidden), "denylisted")
		request.assertNotApplied(test, request.base)
	})
	test.Run("expired", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		dbfx.Exec(test, `UPDATE decision_action SET expires_at = now() - interval '1 minute' WHERE id = $1`, request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "expired")
		request.assertNotApplied(test, request.base)
	})
	test.Run("approver is not an owner", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		makeApproverOwner(test, request.action, "member")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusForbidden), "class_needs_owner")
		request.assertNotApplied(test, request.base)
	})
	test.Run("preview digest absent", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		makeApproverOwner(test, request.action, "owner")
		mustContain(test, request.action.approve(test, withVerifier(acceptingVerifier), request.action.claim(test, map[string]string{}), http.StatusConflict), "preview_required")
		request.assertNotApplied(test, request.base)
	})
	test.Run("no valid passkey assertion", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		request.action.enrollPasskey(test)
		makeApproverOwner(test, request.action, "owner")
		claim := request.claim(test)
		claim["assertion"] = nil
		request.action.approve(test, passkeyHandler(test), claim, http.StatusForbidden)
		request.assertNotApplied(test, request.base)
	})
	test.Run("params point at a workflow of another workspace", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		dbfx.Exec(test, `UPDATE decision_action SET params = jsonb_set(params, '{workflow_id}', to_jsonb($1::text)) WHERE id = $2`, foreignWorkspaceWorkflow(test), request.action.actionID)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "target_workflow_mismatch")
		request.assertNotApplied(test, request.base)
	})
}

func TestConfigChangeApproveRunsOnce(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	handler := withVerifier(acceptingVerifier)
	request.approveOwner(test, handler, http.StatusOK)
	request.action.approve(test, handler, request.claim(test), http.StatusConflict)
	if n := request.action.activityCount(test, "config_change_applied"); n != 1 {
		test.Fatalf("config_change_applied rows = %d after a replay, want 1", n)
	}
}

func TestConfigChangeSecondRequestOnTheSameBaseDrifts(test *testing.T) {
	first, _ := requestConfig(test, http.StatusCreated, nil)
	second, _ := requestConfigOn(test, first.workflowID, first.base)
	handler := withVerifier(acceptingVerifier)
	first.approveOwner(test, handler, http.StatusOK)
	applied := liveStepsDigest(test, first.workflowID)
	mustContain(test, second.approveOwner(test, handler, http.StatusConflict), "precondition_drift")
	second.action.assertUntouched(test)
	if got := liveStepsDigest(test, first.workflowID); got != applied {
		test.Fatalf("the losing request changed the workflow: %s, want %s", got, applied)
	}
}

// requestConfigOn files another request against an existing workflow and base.
func requestConfigOn(test *testing.T, workflowID, base string) (configRequest, string) {
	test.Helper()
	set := map[string]string{"todo": "do A differently"}
	params := map[string]any{"workflow_id": workflowID, "set_instructions": set, "base_sha256": base, "after_sha256": afterDigest(test, workflowID, set)}
	fixture := newActionFixture(test)
	var created actionResponse
	body := fixture.respond(test, fixture.withAction(map[string]any{"type": configAction, "version": 1, "params": params}), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	return configRequest{pendingAction{fixture, created.ID, created.Action.ID}, workflowID, base, set}, body
}

func TestConfigChangeConcurrentApprovalsApplyOnce(test *testing.T) {
	first, _ := requestConfig(test, http.StatusCreated, nil)
	second, _ := requestConfigOn(test, first.workflowID, first.base)
	makeApproverOwner(test, first.action, "owner")
	handler := withVerifier(acceptingVerifier)
	statuses := make([]int, 2)
	var group sync.WaitGroup
	for i, request := range []configRequest{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[i] = approveStatus(test, handler, request)
		}()
	}
	group.Wait()
	applied := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id IN ($1, $2) AND action = 'config_change_applied'`, first.action.issueID, second.action.issueID)
	if applied != 1 {
		test.Fatalf("applied %d times (statuses %v), want exactly 1", applied, statuses)
	}
	if statuses[0] != http.StatusOK && statuses[1] != http.StatusOK {
		test.Fatalf("statuses %v; one approval must succeed", statuses)
	}
}

func approveStatus(test *testing.T, handler *Handler, request configRequest) int {
	test.Helper()
	raw, err := json.Marshal(request.claim(test))
	if err != nil {
		test.Error(err)
		return 0
	}
	call := request.action.decide("approve", request.action.humanID, "interactive_jwt")
	call.Body = readerCloser{bytes.NewReader(raw)}
	recorder := httptest.NewRecorder()
	handler.ApproveDecisionAction(recorder, call)
	return recorder.Code
}

func TestConfigChangeBusyTargetAnswersBusyRetry(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	holdAdvisoryLock(test, configChangeLockKey(testWorkspaceID, "workflow.instructions", request.workflowID))
	mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "busy_retry")
	request.assertNotApplied(test, request.base)
}

// holdAdvisoryLock takes the key's transaction lock on another connection
// until the test ends, as a competing approval would.
func holdAdvisoryLock(test *testing.T, key string) {
	test.Helper()
	holdRow(test, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, key)
}

// ordinaryEdit changes the workflow the way the workflow editor does: a plain
// UPDATE in its own transaction, with a short lock timeout so a blocked write
// fails fast. It reports whether the edit committed.
func ordinaryEdit(test *testing.T, workflowID string) bool {
	test.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		test.Error(err)
		return false
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		test.Error(err)
		return false
	}
	if _, err := tx.Exec(ctx, `UPDATE issue_workflow SET description = 'ordinary edit' WHERE id = $1`, workflowID); err != nil {
		return false
	}
	return tx.Commit(ctx) == nil
}

func TestConfigChangeLocksTheTargetRowBeforeItReads(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	var slipped atomic.Bool
	hook := func() { slipped.Store(ordinaryEdit(test, request.workflowID)) }
	configBeforeApply.Store(&hook)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	if slipped.Load() {
		test.Fatal("an ordinary edit committed in the middle of the approval and the approval then overwrote it")
	}
	if got := liveStepsDigest(test, request.workflowID); got == request.base {
		test.Fatal("approval did not apply")
	}
	var description string
	dbfx.QueryRow(test, `SELECT description FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&description)
	if description == "ordinary edit" {
		test.Fatal("the ordinary edit was written during the approval")
	}
}

func TestConfigChangeRefusesWhileAnOrdinaryEditIsInFlight(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	ctx := context.Background()
	edit, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = edit.Rollback(ctx) })
	if _, err := edit.Exec(ctx, `UPDATE issue_workflow SET description = 'ordinary edit' WHERE id = $1`, request.workflowID); err != nil {
		test.Fatal(err)
	}
	mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "busy_retry")
	request.assertNotApplied(test, request.base)
	if err := edit.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	// The edit left the steps alone, so the approval now applies on the same base.
	request.action.approve(test, withVerifier(acceptingVerifier), request.claim(test), http.StatusOK)
}

func TestConfigChangeHoldsTheRowFromTheStartOfTheApproval(test *testing.T) {
	request, _ := requestConfig(test, http.StatusCreated, nil)
	var committed atomic.Bool
	hook := func() { committed.Store(ordinaryEdit(test, request.workflowID)) }
	configBeforeLock.Store(&hook)
	request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
	// The approval locks the target before the passkey check, so the window
	// before this action's own lock is closed: an ordinary edit cannot commit in it.
	if committed.Load() {
		test.Fatal("an ordinary edit committed before the config change took its lock")
	}
	var description string
	dbfx.QueryRow(test, `SELECT description FROM issue_workflow WHERE id = $1`, request.workflowID).Scan(&description)
	if description == "ordinary edit" {
		test.Fatal("the ordinary edit was written during the approval")
	}
	if liveStepsDigest(test, request.workflowID) == request.base {
		test.Fatal("approval did not apply")
	}
}

func TestConfigChangeFieldIsBoundToTheActionType(test *testing.T) {
	// workspace.context is on the allow list but is not this action's field, so a
	// read that relabels the change must be refused, not just unknown fields.
	test.Run("relabelled at request", func(test *testing.T) {
		registerConfigAction(test)
		configField.Store("workspace.context")
		workflowID, base := configWorkflow(test)
		set := map[string]string{"todo": "x"}
		fixture := newActionFixture(test)
		params := map[string]any{"workflow_id": workflowID, "set_instructions": set, "base_sha256": base, "after_sha256": afterDigest(test, workflowID, set)}
		body := fixture.respond(test, fixture.withAction(map[string]any{"type": configAction, "version": 1, "params": params}), http.StatusForbidden, nil)
		mustContain(test, body, "denylisted")
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("relabelled request stored %d requests and %d actions", requests, actions)
		}
	})
	test.Run("relabelled at approval", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		configField.Store("workspace.context")
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusForbidden), "denylisted")
		request.assertNotApplied(test, request.base)
	})
}

func TestConfigChangeAuthorizeSeesTheStoredProposer(test *testing.T) {
	test.Run("request and approval use the stored identities", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, func(_ string, params map[string]any) {
			params["asking_agent_id"] = "00000000-0000-0000-0000-00000000dead"
			params["proposer"] = "00000000-0000-0000-0000-00000000beef"
		})
		if got := seenProposers(); len(got) != 1 || got[0] != request.action.agentID {
			test.Fatalf("request-time proposers %v, want [%s]", got, request.action.agentID)
		}
		var stored string
		dbfx.QueryRow(test, `SELECT requested_by_agent_id::text FROM decision_action WHERE id = $1`, request.action.actionID).Scan(&stored)
		request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusOK)
		if got := seenProposers(); len(got) != 2 || got[1] != stored || stored != request.action.agentID {
			test.Fatalf("proposers %v, want [%s %s]", got, request.action.agentID, stored)
		}
	})
	test.Run("refused at request", func(test *testing.T) {
		registerConfigAction(test)
		refuse := func(pgtype.UUID) error {
			return decisionIntentRefusal{http.StatusForbidden, "proposer_unauthorized: proposer is not authorized"}
		}
		configAuthorize.Store(&refuse)
		workflowID, base := configWorkflow(test)
		set := map[string]string{"todo": "x"}
		fixture := newActionFixture(test)
		params := map[string]any{"workflow_id": workflowID, "set_instructions": set, "base_sha256": base, "after_sha256": afterDigest(test, workflowID, set)}
		body := fixture.respond(test, fixture.withAction(map[string]any{"type": configAction, "version": 1, "params": params}), http.StatusForbidden, nil)
		mustContain(test, body, "proposer_unauthorized")
		if requests, actions := fixture.requestCounts(test); requests != 0 || actions != 0 {
			test.Fatalf("refused request stored %d requests and %d actions", requests, actions)
		}
	})
	test.Run("refused at approval", func(test *testing.T) {
		request, _ := requestConfig(test, http.StatusCreated, nil)
		revoke := func(pgtype.UUID) error {
			return decisionIntentRefusal{http.StatusConflict, "proposer_revoked: proposer lost the grant"}
		}
		configAuthorize.Store(&revoke)
		mustContain(test, request.approveOwner(test, withVerifier(acceptingVerifier), http.StatusConflict), "proposer_revoked")
		request.assertNotApplied(test, request.base)
	})
}

func TestAuthorizeConfigChangeFailsClosedOnAnInvalidProposer(test *testing.T) {
	called := false
	change := configChange{Authorize: func(context.Context, *db.Queries, pgtype.UUID, pgtype.UUID, json.RawMessage, configChangeRead) error {
		called = true
		return nil
	}}
	err := authorizeConfigChange(context.Background(), change, nil, pgtype.UUID{}, pgtype.UUID{}, nil, configChangeRead{})
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || !strings.HasPrefix(refusal.message, "proposer_required") || called {
		test.Fatalf("err = %v called = %v; want proposer_required and no hook call", err, called)
	}
	if err := authorizeConfigChange(context.Background(), configChange{}, nil, pgtype.UUID{}, pgtype.UUID{}, nil, configChangeRead{}); err != nil {
		test.Fatalf("no hook: %v", err)
	}
}
