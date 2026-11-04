package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

const testEndpointID = "33333333-3333-3333-3333-333333333333"

type webhookCall struct{ Method, Path, Body string }

// webhookServer answers the management API and records every request.
func webhookServer(t *testing.T, status int) (*httptest.Server, func() []webhookCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []webhookCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, webhookCall{r.Method, r.URL.Path, string(body)})
		mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		base := "/api/issue-workflows/" + testWorkflowID + "/webhooks"
		ep := map[string]any{"id": testEndpointID, "name": "ci", "rate_per_min": 60, "created_at": "2026-10-28T00:00:00Z"}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			_ = json.NewEncoder(w).Encode([]any{ep})
		case r.Method == http.MethodPost && r.URL.Path == base:
			ep["secret"] = "whs_created"
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ep)
		case r.Method == http.MethodPost && r.URL.Path == base+"/"+testEndpointID+"/rotate":
			ep["secret"] = "whs_rotated"
			_ = json.NewEncoder(w).Encode(ep)
		case r.Method == http.MethodPost && r.URL.Path == base+"/"+testEndpointID+"/disable":
			ep["disabled_at"] = "2026-10-28T01:00:00Z"
			_ = json.NewEncoder(w).Encode(ep)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setCLITestServerEnv(t, srv.URL)
	return srv, func() []webhookCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]webhookCall(nil), calls...)
	}
}

func newWebhookTestCmd() *cobra.Command {
	cmd := newWorkflowTestCmd("name")
	cmd.Flags().Int("rate-per-min", 0, "")
	return cmd
}

func TestWorkflowWebhookCreateSendsNameAndPrintsSecret(t *testing.T) {
	_, calls := webhookServer(t, http.StatusOK)
	cmd := newWebhookTestCmd()
	_ = cmd.Flags().Set("name", "ci")
	_ = cmd.Flags().Set("rate-per-min", "30")
	out, err := captureStdout(t, func() error { return runWorkflowWebhookCreate(cmd, []string{testWorkflowID}) })
	if err != nil {
		t.Fatal(err)
	}
	got := calls()
	if len(got) != 1 || got[0].Method != http.MethodPost {
		t.Fatalf("calls = %+v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(got[0].Body), &sent); err != nil || sent["name"] != "ci" || sent["rate_per_min"] != float64(30) {
		t.Fatalf("body = %s (%v)", got[0].Body, err)
	}
	if !strings.Contains(out, "whs_created") {
		t.Fatalf("the secret is not printed: %s", out)
	}
	_ = cmd.Flags().Set("output", "table")
	out, err = captureStdout(t, func() error { return runWorkflowWebhookCreate(cmd, []string{testWorkflowID}) })
	if err != nil || !strings.Contains(out, "whs_created") || !strings.Contains(strings.ToLower(out), "only time") {
		t.Fatalf("table output must show the secret with a shown-once note: %v\n%s", err, out)
	}
}

func TestWorkflowWebhookCreateNeedsAName(t *testing.T) {
	_, calls := webhookServer(t, http.StatusOK)
	err := runWorkflowWebhookCreate(newWebhookTestCmd(), []string{testWorkflowID})
	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v", err)
	}
	if len(calls()) != 0 {
		t.Fatalf("a request was sent: %+v", calls())
	}
}

func TestWorkflowWebhookRotateAndDisableResolveTheName(t *testing.T) {
	_, calls := webhookServer(t, http.StatusOK)
	cmd := newWebhookTestCmd()
	_ = cmd.Flags().Set("name", "ci")
	out, err := captureStdout(t, func() error { return runWorkflowWebhookRotate(cmd, []string{testWorkflowID}) })
	if err != nil || !strings.Contains(out, "whs_rotated") {
		t.Fatalf("rotate: %v\n%s", err, out)
	}
	if _, err := captureStdout(t, func() error { return runWorkflowWebhookDisable(cmd, []string{testWorkflowID}) }); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range calls() {
		paths = append(paths, c.Method+" "+strings.TrimPrefix(c.Path, "/api/issue-workflows/"+testWorkflowID+"/webhooks"))
	}
	want := "GET  | POST /" + testEndpointID + "/rotate | GET  | POST /" + testEndpointID + "/disable"
	if strings.Join(paths, " | ") != want {
		t.Fatalf("calls = %v", paths)
	}
}

func TestWorkflowWebhookUnknownNameSendsNoChange(t *testing.T) {
	_, calls := webhookServer(t, http.StatusOK)
	cmd := newWebhookTestCmd()
	_ = cmd.Flags().Set("name", "missing")
	for _, run := range []func(*cobra.Command, []string) error{runWorkflowWebhookRotate, runWorkflowWebhookDisable} {
		err := run(cmd, []string{testWorkflowID})
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("err = %v", err)
		}
	}
	for _, c := range calls() {
		if c.Method != http.MethodGet {
			t.Fatalf("a change was sent: %+v", c)
		}
	}
}

func TestWorkflowWebhookListHasNoSecretAndExplains403(t *testing.T) {
	_, _ = webhookServer(t, http.StatusOK)
	cmd := newWebhookTestCmd()
	_ = cmd.Flags().Set("output", "table")
	out, err := captureStdout(t, func() error { return runWorkflowWebhookList(cmd, []string{testWorkflowID}) })
	if err != nil || !strings.Contains(out, "ci") || strings.Contains(strings.ToLower(out), "secret") {
		t.Fatalf("list: %v\n%s", err, out)
	}

	_, _ = webhookServer(t, http.StatusForbidden)
	err = runWorkflowWebhookDisable(func() *cobra.Command { c := newWebhookTestCmd(); _ = c.Flags().Set("name", "ci"); return c }(), []string{testWorkflowID})
	if err == nil || !strings.Contains(err.Error(), "owner or admin") {
		t.Fatalf("err = %v", err)
	}
}
