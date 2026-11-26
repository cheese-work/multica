package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/plugincontract"
)

// guardTestInstallation is an installation whose only hook is a guard hook. The
// ID is fresh per call because the breaker and the rate limit count rows by
// installation, and the tests share one database.
func guardTestInstallation(t *testing.T, endpoint, netScope, onError string, timeoutMs int) db.PluginInstallation {
	t.Helper()
	manifest := fmt.Sprintf(`{
		"manifest_version": 1,
		"key": "com.example.guardtest",
		"name": "Guard Test",
		"description": "d",
		"version": "1.0.0",
		"author": {"name": "example"},
		"scopes": ["issues:read", "net:%s"],
		"contributes": {"hooks": [{
			"key": "qa-proof",
			"name": "QA proof",
			"description": "Allow or deny the move.",
			"triggers": ["guard"],
			"guard": {"timeout_ms": %d, "on_error": %q, "overridable": true, "metadata_writers": ["post_close_work"]},
			"transport": {"type": "http", "url": "%s"}
		}]}
	}`, netScope, timeoutMs, onError, endpoint)
	if _, _, err := plugincontract.ParseManifest([]byte(manifest)); err != nil {
		t.Fatalf("guard test manifest must be valid: %v", err)
	}
	scopes, err := json.Marshal([]string{"issues:read", "net:" + netScope})
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	return db.PluginInstallation{
		ID:            pgtype.UUID{Bytes: uuid.New(), Valid: true},
		WorkspaceID:   pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Enabled:       true,
		Manifest:      []byte(manifest),
		GrantedScopes: scopes,
	}
}

type guardFixture struct {
	harness      *hookTestServer
	service      *PluginService
	installation db.PluginInstallation
	invocation   HookInvocation
}

func newGuardFixture(t *testing.T, onError string, timeoutMs int, respond func(w http.ResponseWriter, body []byte)) guardFixture {
	t.Helper()
	harness := newHookTestServer(t)
	harness.respond = respond
	service := hookTestService(t, harness)
	service.Queries = db.New(sharedTestPool(t))
	installation := guardTestInstallation(t, harness.server.URL+"/hooks/qa-proof", hookTestHost(harness), onError, timeoutMs)
	hook, err := FindHook(installation, "qa-proof")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	return guardFixture{
		harness:      harness,
		service:      service,
		installation: installation,
		invocation: HookInvocation{
			Installation: installation,
			Hook:         hook,
			Actor:        HookActor{Type: "member", ID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
			IssueID:      pgtype.UUID{Bytes: uuid.New(), Valid: true},
			Input:        map[string]any{"from": "in_progress", "to": "done"},
		},
	}
}

func (f guardFixture) requests() int { return len(f.harness.received) }

func answer(body string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func guardRow(t *testing.T, f guardFixture, id pgtype.UUID) (trigger, status string) {
	t.Helper()
	err := sharedTestPool(t).QueryRow(context.Background(),
		`SELECT trigger, status FROM plugin_invocation WHERE id = $1`, id).Scan(&trigger, &status)
	if err != nil {
		t.Fatalf("the guard call must leave an invocation row: %v", err)
	}
	return trigger, status
}

func TestInvokeGuardAllow(t *testing.T) {
	f := newGuardFixture(t, "closed", 2000, answer(`{"decision":"allow"}`))
	result, err := f.service.InvokeGuard(context.Background(), f.invocation)
	if err != nil {
		t.Fatalf("InvokeGuard: %v", err)
	}
	if result.Decision != GuardDecisionAllow || result.Source != GuardSourcePlugin || result.Status != "ok" {
		t.Fatalf("result = %+v, want a plugin allow", result)
	}
	var body hookRequestBody
	if err := json.Unmarshal((<-f.harness.received).Body, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Trigger != plugincontract.TriggerGuard || body.HookKey != "qa-proof" || body.IssueID == "" {
		t.Fatalf("guard request body = %+v", body)
	}
	trigger, status := guardRow(t, f, result.InvocationID)
	if trigger != "guard" || status != "ok" {
		t.Fatalf("invocation row = (%s, %s), want (guard, ok)", trigger, status)
	}
}

func TestInvokeGuardDenyCarriesTheReason(t *testing.T) {
	f := newGuardFixture(t, "open", 2000, answer(`{"decision":"deny","reason":"no QA line on the issue"}`))
	result, err := f.service.InvokeGuard(context.Background(), f.invocation)
	if err != nil {
		t.Fatalf("InvokeGuard: %v", err)
	}
	// on_error: open must not soften an explicit deny.
	if result.Decision != GuardDecisionDeny || result.Reason != "no QA line on the issue" || result.Source != GuardSourcePlugin {
		t.Fatalf("result = %+v, want a plugin deny with its reason", result)
	}
}

func TestInvokeGuardFailureFollowsOnError(t *testing.T) {
	cases := []struct {
		name    string
		timeout int
		respond func(http.ResponseWriter, []byte)
		status  string
	}{
		{name: "timeout", timeout: 100, status: "timeout", respond: func(w http.ResponseWriter, _ []byte) {
			time.Sleep(600 * time.Millisecond)
			_, _ = w.Write([]byte(`{"decision":"allow"}`))
		}},
		{name: "5xx", timeout: 2000, status: "failed", respond: func(w http.ResponseWriter, _ []byte) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}},
		{name: "not json", timeout: 2000, status: "failed", respond: answer(`<html>`)},
		{name: "empty body", timeout: 2000, status: "failed", respond: func(w http.ResponseWriter, _ []byte) {}},
		{name: "unknown decision", timeout: 2000, status: "failed", respond: answer(`{"decision":"maybe"}`)},
		{name: "missing decision", timeout: 2000, status: "failed", respond: answer(`{"reason":"x"}`)},
		{name: "deny without reason", timeout: 2000, status: "failed", respond: answer(`{"decision":"deny"}`)},
		{name: "reason over 500 chars", timeout: 2000, status: "failed", respond: answer(`{"decision":"deny","reason":"` + strings.Repeat("x", 501) + `"}`)},
		{name: "array body", timeout: 2000, status: "failed", respond: answer(`["allow"]`)},
	}
	for _, tc := range cases {
		for _, onError := range []string{"closed", "open"} {
			t.Run(tc.name+"/"+onError, func(t *testing.T) {
				f := newGuardFixture(t, onError, tc.timeout, tc.respond)
				result, err := f.service.InvokeGuard(context.Background(), f.invocation)
				if err != nil {
					t.Fatalf("a failed call is a result, not an error: %v", err)
				}
				if result.Source != GuardSourceOnError || result.Status != tc.status {
					t.Fatalf("result = %+v, want an on_error answer with status %s", result, tc.status)
				}
				if onError == "closed" {
					if result.Decision != GuardDecisionDeny || result.Reason != "guard plugin unavailable: com.example.guardtest" {
						t.Fatalf("closed must refuse with a named reason, got %+v", result)
					}
				} else if result.Decision != GuardDecisionAllow {
					t.Fatalf("open must allow, got %+v", result)
				}
				if _, status := guardRow(t, f, result.InvocationID); status != tc.status {
					t.Fatalf("row status = %s, want %s", status, tc.status)
				}
			})
		}
	}
}

// The wait is bounded by the guard's own timeout, not the 10 s hook default.
func TestInvokeGuardIsBoundedByTheGuardTimeout(t *testing.T) {
	f := newGuardFixture(t, "closed", 100, func(w http.ResponseWriter, _ []byte) {
		time.Sleep(2 * time.Second)
	})
	started := time.Now()
	result, err := f.service.InvokeGuard(context.Background(), f.invocation)
	if err != nil {
		t.Fatalf("InvokeGuard: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("guard waited %s; the 100 ms guard timeout must bound it", elapsed)
	}
	if result.Decision != GuardDecisionDeny || result.Status != "timeout" {
		t.Fatalf("result = %+v", result)
	}
}

// An open breaker answers from on_error without touching the network.
func TestInvokeGuardOpenBreakerMakesNoNetworkCall(t *testing.T) {
	for _, onError := range []string{"closed", "open"} {
		t.Run(onError, func(t *testing.T) {
			f := newGuardFixture(t, onError, 2000, answer(`{"decision":"allow"}`))
			for i := 0; i < hookBreakerThreshold; i++ {
				if _, err := f.service.Queries.CreatePluginInvocation(context.Background(), db.CreatePluginInvocationParams{
					InstallationID: f.installation.ID,
					WorkspaceID:    f.installation.WorkspaceID,
					HookKey:        "qa-proof",
					Trigger:        "guard",
					Status:         "failed",
					Attempt:        1,
				}); err != nil {
					t.Fatalf("seed failure %d: %v", i, err)
				}
			}
			result, err := f.service.InvokeGuard(context.Background(), f.invocation)
			if err != nil {
				t.Fatalf("InvokeGuard: %v", err)
			}
			if f.requests() != 0 {
				t.Fatal("an open breaker must not make a network call")
			}
			if result.Source != GuardSourceOnError || result.Called {
				t.Fatalf("result = %+v, want an on_error answer with no call", result)
			}
			wantDecision := GuardDecisionDeny
			if onError == "open" {
				wantDecision = GuardDecisionAllow
			}
			if result.Decision != wantDecision {
				t.Fatalf("decision = %s, want %s", result.Decision, wantDecision)
			}
		})
	}
}

func TestInvokeGuardDisabledInstallationMakesNoCall(t *testing.T) {
	f := newGuardFixture(t, "closed", 2000, answer(`{"decision":"allow"}`))
	f.invocation.Installation.Enabled = false
	result, err := f.service.InvokeGuard(context.Background(), f.invocation)
	if err != nil {
		t.Fatalf("InvokeGuard: %v", err)
	}
	if f.requests() != 0 || result.Decision != GuardDecisionDeny || result.Source != GuardSourceOnError {
		t.Fatalf("result = %+v, requests = %d", result, f.requests())
	}
}

func TestInvokeGuardRefusesAHookThatIsNotAGuard(t *testing.T) {
	harness := newHookTestServer(t)
	service := hookTestService(t, harness)
	service.Queries = db.New(sharedTestPool(t))
	installation := hookTestInstallation(t, harness.server.URL+"/hooks/summarize", hookTestHost(harness), []string{plugincontract.TriggerManual})
	hook, err := FindHook(installation, "summarize")
	if err != nil {
		t.Fatalf("find hook: %v", err)
	}
	if _, err := service.InvokeGuard(context.Background(), HookInvocation{Installation: installation, Hook: hook}); err == nil {
		t.Fatal("a hook without the guard trigger must not be callable as a guard")
	}
	if len(harness.received) != 0 {
		t.Fatal("no request may leave for a non-guard hook")
	}
}

// What a handler can do with its callback token during a guard call: the grant
// carries the guard trigger, which is what the Action API checks to go
// read-only.
func TestInvokeGuardCallbackGrantCarriesTheGuardTrigger(t *testing.T) {
	var seen CallbackGrant
	var f guardFixture
	f = newGuardFixture(t, "closed", 2000, func(w http.ResponseWriter, body []byte) {
		var request hookRequestBody
		_ = json.Unmarshal(body, &request)
		grant, err := f.service.Callbacks.Resolve(request.CallbackToken)
		if err == nil {
			seen = grant
		}
		_, _ = w.Write([]byte(`{"decision":"allow"}`))
	})
	if _, err := f.service.InvokeGuard(context.Background(), f.invocation); err != nil {
		t.Fatalf("InvokeGuard: %v", err)
	}
	if seen.Trigger != plugincontract.TriggerGuard {
		t.Fatalf("grant trigger = %q, want guard", seen.Trigger)
	}
}
