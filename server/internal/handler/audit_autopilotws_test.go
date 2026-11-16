package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CHE-5648 (E3 of CHE-4807): autopilot collaborator, delivery, trigger and
// webhook-secret routes plus the first workspace-config routes record an audit
// row in the mutation's transaction. One table drives every wired route through
// a success test and a rollback test.

// Built at run time so the file holds no secret-shaped literal for gitleaks.
var apwsSecret = strings.Join([]string{"apws", "signing", "value", "0123456789"}, "-")

// apwsRun is one prepared request. applied reports whether the mutation took
// effect, so the rollback test can prove nothing was applied.
type apwsRun struct {
	workspaceID string
	targetID    string
	actorID     string // defaults to the shared test user
	secrets     []string
	call        func() *httptest.ResponseRecorder
	applied     func() bool
	check       func(t *testing.T, row auditRow, body string)
}

type apwsCase struct {
	name       string
	kind       string
	targetType string
	wantStatus int
	setup      func(t *testing.T) apwsRun
}

func apwsRecord(handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func apwsApplied(t *testing.T, query string, args ...any) func() bool {
	return func() bool {
		var n int
		dbfx.QueryRow(t, query, args...).Scan(&n)
		return n > 0
	}
}

func apwsTrigger(t *testing.T) (apID string, trigger AutopilotTriggerResponse) {
	agentID := createWebhookTestAgent(t, "apws-agent")
	apID = createWebhookTestAutopilot(t, agentID, "active", "run_only")
	return apID, createWebhookTriggerViaHandler(t, apID)
}

func apwsCases() []apwsCase {
	return []apwsCase{
		{
			name: "collaborator-add", kind: "autopilot.collaborator.add", targetType: "autopilot", wantStatus: http.StatusCreated,
			setup: func(t *testing.T) apwsRun {
				apID := createWebhookTestAutopilot(t, createWebhookTestAgent(t, "apws-collab-add"), "active", "run_only")
				memberID, _ := addWorkflowTestMember(t, "apws-collab-add")
				req := withURLParam(newRequest("POST", "/api/autopilots/"+apID+"/collaborators", map[string]any{"user_id": memberID}), "id", apID)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: apID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.AddAutopilotCollaborator, req) },
					applied: apwsApplied(t, `SELECT count(*) FROM autopilot_collaborator WHERE autopilot_id = $1 AND user_id = $2`, apID, memberID),
					check: func(t *testing.T, row auditRow, _ string) {
						if row.After["user_id"] != memberID {
							t.Errorf("after = %v; want user_id %s", row.After, memberID)
						}
					},
				}
			},
		},
		{
			name: "collaborator-remove", kind: "autopilot.collaborator.remove", targetType: "autopilot", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				apID := createWebhookTestAutopilot(t, createWebhookTestAgent(t, "apws-collab-rm"), "active", "run_only")
				memberID, _ := addWorkflowTestMember(t, "apws-collab-rm")
				dbfx.Exec(t, `INSERT INTO autopilot_collaborator (autopilot_id, user_type, user_id, granted_by) VALUES ($1, 'member', $2, $3)`, apID, memberID, testUserID)
				req := withURLParams(newRequest("DELETE", "/api/autopilots/"+apID+"/collaborators/"+memberID, nil), "id", apID, "userId", memberID)
				stillGranted := apwsApplied(t, `SELECT count(*) FROM autopilot_collaborator WHERE autopilot_id = $1 AND user_id = $2`, apID, memberID)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: apID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.RemoveAutopilotCollaborator, req) },
					applied: func() bool { return !stillGranted() },
					check: func(t *testing.T, row auditRow, _ string) {
						if row.Before["user_id"] != memberID {
							t.Errorf("before = %v; want user_id %s", row.Before, memberID)
						}
					},
				}
			},
		},
		{
			name: "delivery-replay", kind: "autopilot.delivery.replay", targetType: "autopilot", wantStatus: http.StatusAccepted,
			setup: func(t *testing.T) apwsRun {
				apID, trigger := apwsTrigger(t)
				originalID := requireAcceptedWebhookResponse(t, postWebhook(t, *trigger.WebhookToken, map[string]any{"hello": "world"}, nil))
				processQueuedWebhookDelivery(t, originalID)
				req := withURLParams(newRequest("POST", fmt.Sprintf("/api/autopilots/%s/deliveries/%s/replay", apID, originalID), nil),
					"id", apID, "deliveryId", originalID)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: apID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.ReplayAutopilotDelivery, req) },
					applied: apwsApplied(t, `SELECT count(*) FROM webhook_delivery WHERE replayed_from_delivery_id = $1`, originalID),
					check: func(t *testing.T, row auditRow, body string) {
						var resp map[string]any
						json.Unmarshal([]byte(body), &resp)
						if row.After["delivery_id"] != resp["id"] || row.After["replayed_from_delivery_id"] != originalID {
							t.Errorf("after = %v; want delivery %v replayed from %s", row.After, resp["id"], originalID)
						}
					},
				}
			},
		},
		{
			name: "trigger-now", kind: "autopilot.trigger.manual", targetType: "autopilot", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				apID := createWebhookTestAutopilot(t, createWebhookTestAgent(t, "apws-run-now"), "active", "run_only")
				dbfx.Cleanup(t, `DELETE FROM autopilot_run WHERE autopilot_id = $1`, apID)
				req := withURLParam(newRequest("POST", "/api/autopilots/"+apID+"/trigger", nil), "id", apID)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: apID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.TriggerAutopilot, req) },
					applied: apwsApplied(t, `SELECT count(*) FROM autopilot_run WHERE autopilot_id = $1`, apID),
				}
			},
		},
		{
			name: "rotate-webhook-token", kind: "autopilot.trigger.rotate_token", targetType: "autopilot_trigger", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				apID, trigger := apwsTrigger(t)
				oldToken := *trigger.WebhookToken
				req := withURLParams(newRequest("POST", fmt.Sprintf("/api/autopilots/%s/triggers/%s/rotate-webhook-token", apID, trigger.ID), nil),
					"id", apID, "triggerId", trigger.ID)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: trigger.ID, secrets: []string{oldToken},
					call: func() *httptest.ResponseRecorder {
						return apwsRecord(testHandler.RotateAutopilotTriggerWebhookToken, req)
					},
					applied: apwsApplied(t, `SELECT count(*) FROM autopilot_trigger WHERE id = $1 AND webhook_token <> $2`, trigger.ID, oldToken),
					check: func(t *testing.T, row auditRow, body string) {
						var resp AutopilotTriggerResponse
						json.Unmarshal([]byte(body), &resp)
						if resp.WebhookToken == nil {
							t.Fatalf("rotate response has no token: %s", body)
						}
						apjAssertNoLeak(t, row, *resp.WebhookToken)
					},
				}
			},
		},
		{
			name: "signing-secret", kind: "autopilot.trigger.signing_secret", targetType: "autopilot_trigger", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				apID, trigger := apwsTrigger(t)
				req := withURLParams(newRequest("PUT", fmt.Sprintf("/api/autopilots/%s/triggers/%s/signing-secret", apID, trigger.ID),
					map[string]any{"signing_secret": apwsSecret}), "id", apID, "triggerId", trigger.ID)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: trigger.ID, secrets: []string{apwsSecret, apwsSecret[len(apwsSecret)-4:]},
					call: func() *httptest.ResponseRecorder {
						return apwsRecord(testHandler.SetAutopilotTriggerSigningSecret, req)
					},
					applied: apwsApplied(t, `SELECT count(*) FROM autopilot_trigger WHERE id = $1 AND signing_secret IS NOT NULL`, trigger.ID),
				}
			},
		},
		{
			name: "decision-gate-set", kind: "workspace.decision_gate.update", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				f := newGateFixture(t, 2)
				body := map[string]any{"state": "block", "assertion": f.sign(0, f.issue("block"))}
				req := f.gateRequest("", f.owner, "interactive_jwt", body)
				return apwsRun{
					workspaceID: f.workspace, targetID: f.workspace, actorID: f.owner,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(f.handler.SetDecisionGate, req) },
					applied: func() bool { state, _ := f.gate(); return state == "block" },
					check: func(t *testing.T, row auditRow, _ string) {
						if row.Before["decision_passkey_gate"] != "off" || row.After["decision_passkey_gate"] != "block" {
							t.Errorf("diff = %v -> %v; want gate off -> block", row.Before, row.After)
						}
					},
				}
			},
		},
		{
			name: "routing-agent-set", kind: "workspace.routing_agent.update", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				wsID, primary, _, _ := routingFixture(t, "apws-routing")
				req := withURLParam(newRequest("PUT", "/api/workspaces/"+wsID+"/routing-agent", map[string]any{"agent_id": primary}), "id", wsID)
				return apwsRun{
					workspaceID: wsID, targetID: wsID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.UpdateRoutingAgent, req) },
					applied: apwsApplied(t, `SELECT count(*) FROM workspace WHERE id = $1 AND settings->'background_agents'->'routing' IS NOT NULL`, wsID),
					check: func(t *testing.T, row auditRow, _ string) {
						if row.After["agent_id"] != primary {
							t.Errorf("after = %v; want agent_id %s", row.After, primary)
						}
					},
				}
			},
		},
		{
			name: "background-agents-put", kind: "workspace.background_agents.update", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				wsID, a, _, _ := bgFixture(t, "apws-bg-put")
				req := withURLParam(newRequest("PUT", "/api/workspaces/"+wsID+"/background-agents", map[string]any{"dm_title": map[string]any{"agent_id": a}}), "id", wsID)
				return apwsRun{
					workspaceID: wsID, targetID: wsID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.UpdateBackgroundAgents, req) },
					applied: apwsApplied(t, `SELECT count(*) FROM workspace WHERE id = $1 AND settings->'background_agents' IS NOT NULL`, wsID),
					check: func(t *testing.T, row auditRow, _ string) {
						if _, ok := row.After["dm_title"]; !ok {
							t.Errorf("after = %v; want the changed dm_title job named", row.After)
						}
					},
				}
			},
		},
		{
			name: "background-agents-delete", kind: "workspace.background_agents.reset", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				wsID, a, _, _ := bgFixture(t, "apws-bg-del")
				putBG(t, wsID, map[string]any{"dm_title": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
				req := withURLParam(newRequest("DELETE", "/api/workspaces/"+wsID+"/background-agents", nil), "id", wsID)
				stillSet := apwsApplied(t, `SELECT count(*) FROM workspace WHERE id = $1 AND settings->'background_agents' IS NOT NULL`, wsID)
				return apwsRun{
					workspaceID: wsID, targetID: wsID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.ResetBackgroundAgents, req) },
					applied: func() bool { return !stillSet() },
					check: func(t *testing.T, row auditRow, _ string) {
						if _, ok := row.Before["dm_title"]; !ok {
							t.Errorf("before = %v; want the cleared dm_title job named", row.Before)
						}
					},
				}
			},
		},
		{
			name: "workspace-pause", kind: "workspace.scheduling.pause", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				wsID := pauseWorkspaceFixture(t, "apws-pause")
				req := withURLParam(newRequest("POST", "/api/workspaces/"+wsID+"/pause", nil), "id", wsID)
				return apwsRun{
					workspaceID: wsID, targetID: wsID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.PauseWorkspaceScheduling, req) },
					applied: apwsApplied(t, `SELECT count(*) FROM workspace WHERE id = $1 AND scheduling_paused_at IS NOT NULL`, wsID),
				}
			},
		},
		{
			name: "workspace-resume", kind: "workspace.scheduling.resume", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				wsID := pauseWorkspaceFixture(t, "apws-resume")
				dbfx.Exec(t, `UPDATE workspace SET scheduling_paused_at = now(), scheduling_paused_by = $2 WHERE id = $1`, wsID, testUserID)
				req := withURLParam(newRequest("POST", "/api/workspaces/"+wsID+"/resume", nil), "id", wsID)
				stillPaused := apwsApplied(t, `SELECT count(*) FROM workspace WHERE id = $1 AND scheduling_paused_at IS NOT NULL`, wsID)
				return apwsRun{
					workspaceID: wsID, targetID: wsID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.ResumeWorkspaceScheduling, req) },
					applied: func() bool { return !stillPaused() },
				}
			},
		},
		{
			name: "provider-breaker-reset", kind: "workspace.provider_breaker.reset", targetType: "workspace", wantStatus: http.StatusOK,
			setup: func(t *testing.T) apwsRun {
				const provider = "apws-reset"
				seedProviderBreaker(t, provider, "open")
				req := resetBreakerRequest(testUserID, provider)
				return apwsRun{
					workspaceID: testWorkspaceID, targetID: testWorkspaceID,
					call:    func() *httptest.ResponseRecorder { return apwsRecord(testHandler.ResetProviderBreaker, req) },
					applied: func() bool { return breakerState(t, provider) == "closed" },
					check: func(t *testing.T, row auditRow, _ string) {
						if row.After["provider"] != provider || row.Before["state"] != "open" {
							t.Errorf("row = %v -> %v; want provider %s reset from open", row.Before, row.After, provider)
						}
					},
				}
			},
		},
	}
}

func TestAutopilotWorkspaceRoutes_WriteOneAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range apwsCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.setup(t)
			// Rows from earlier runs may already sit on a shared target.
			base := len(auditRowsFor(t, tc.kind, run.targetID))
			w := run.call()
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			rows := auditRowsFor(t, tc.kind, run.targetID)
			if len(rows)-base != 1 {
				t.Fatalf("%s audit rows for %s = %d; want 1", tc.kind, run.targetID, len(rows)-base)
			}
			row := rows[base]
			actorID := run.actorID
			if actorID == "" {
				actorID = testUserID
			}
			if row.ActorType != "member" || row.ActorID != actorID || row.TargetType != tc.targetType {
				t.Errorf("row = %+v; want %s by member %s on %s", row, tc.kind, actorID, tc.targetType)
			}
			apjAssertNoLeak(t, row, run.secrets...)
			if run.check != nil {
				run.check(t, row, w.Body.String())
			}
			if !run.applied() {
				t.Errorf("%s: audit row written but the mutation is not applied", tc.name)
			}
		})
	}
}

func TestAutopilotWorkspaceRoutes_AuditInsertFailureRollsBack(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range apwsCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.setup(t)
			failAuditInsertForKind(t, tc.kind, run.workspaceID)
			base := len(auditRowsFor(t, tc.kind, run.targetID))
			w := run.call()
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 with a failing audit insert: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Retry-After") == "" {
				t.Error("500 from a failed audit insert must carry Retry-After")
			}
			if run.applied() {
				t.Errorf("%s: mutation applied despite the failed audit insert", tc.name)
			}
			if n := len(auditRowsFor(t, tc.kind, run.targetID)) - base; n != 0 {
				t.Errorf("%s new audit rows = %d after a failed insert; want 0", tc.kind, n)
			}
		})
	}
}

// The decision-gate challenge issues a one-time assertion challenge and changes
// no workspace state, so it is exempt from the audit trail (see
// auditExemptRoutes). The gate change itself is audited above.
func TestDecisionGateChallenge_WritesNoAuditRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	f := newGateFixture(t, 2)
	f.challenge("block", http.StatusOK)
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM workspace_audit_event WHERE workspace_id = $1`, f.workspace).Scan(&n)
	if n != 0 {
		t.Fatalf("challenge wrote %d audit rows; want 0", n)
	}
}
