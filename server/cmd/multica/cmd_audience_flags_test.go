package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const audienceMember = "66666666-6666-4666-8666-666666666666"

func TestAudienceFlagsRegistered(t *testing.T) {
	for _, name := range []string{"ask", "no-ask"} {
		if issueCommentAddCmd.Flags().Lookup(name) == nil {
			t.Errorf("comment add lacks --%s", name)
		}
	}
	if issueStatusCmd.Flags().Lookup("waiting-on") == nil {
		t.Error("issue status lacks --waiting-on")
	}
}

func TestAudienceHelpSharesOneFixLine(t *testing.T) {
	for name, long := range map[string]string{"comment add": issueCommentAddCmd.Long, "status": issueStatusCmd.Long} {
		if !strings.Contains(long, audienceFixLine) {
			t.Errorf("%s help lacks the shared fix line", name)
		}
	}
	if !strings.Contains(audienceFixLine, "--notify-only") {
		t.Errorf("fix line must keep the S2 --notify-only route: %q", audienceFixLine)
	}
}

func commentBodyFor(t *testing.T, setup func(cmd interface{ Set(string, string) error })) map[string]any {
	t.Helper()
	const issueID = "11111111-1111-4111-8111-111111111111"
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID, "identifier": "TST-1"})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": issueID})
	}))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	cmd := newIssueCommentAddTestCmd()
	cmd.Flags().String("ask", "", "")
	cmd.Flags().Bool("no-ask", false, "")
	setup(cmd.Flags())
	_ = cmd.Flags().Set("content", "Waiting on you")
	if _, err := captureStdout(t, func() error { return runIssueCommentAdd(cmd, []string{issueID}) }); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestIssueCommentAskBody(t *testing.T) {
	body := commentBodyFor(t, func(f interface{ Set(string, string) error }) { _ = f.Set("ask", audienceMember) })
	if body["ask"] != audienceMember {
		t.Fatalf("body %+v, want ask %s", body, audienceMember)
	}
	if _, has := body["no_ask"]; has {
		t.Fatalf("body %+v must not carry no_ask", body)
	}
}

func TestIssueCommentNoAskBody(t *testing.T) {
	body := commentBodyFor(t, func(f interface{ Set(string, string) error }) { _ = f.Set("no-ask", "true") })
	if body["no_ask"] != true {
		t.Fatalf("body %+v, want no_ask true", body)
	}
	if _, has := body["ask"]; has {
		t.Fatalf("body %+v must not carry ask", body)
	}
}

func TestIssueCommentPlainBodyHasNoAudience(t *testing.T) {
	body := commentBodyFor(t, func(f interface{ Set(string, string) error }) {})
	for _, k := range []string{"ask", "no_ask"} {
		if _, has := body[k]; has {
			t.Fatalf("plain comment body %+v carries %s", body, k)
		}
	}
}

func TestIssueCommentAskRefusals(t *testing.T) {
	cases := map[string]func(cmd interface{ Set(string, string) error }){
		"ask with no-ask": func(f interface{ Set(string, string) error }) {
			_ = f.Set("ask", audienceMember)
			_ = f.Set("no-ask", "true")
		},
		"ask not a uuid": func(f interface{ Set(string, string) error }) { _ = f.Set("ask", "cheese") },
		"ask blank":      func(f interface{ Set(string, string) error }) { _ = f.Set("ask", "  ") },
		"ask with notify": func(f interface{ Set(string, string) error }) {
			_ = f.Set("ask", audienceMember)
			_ = f.Set("notify-only", "true")
		},
		"no-ask with notify": func(f interface{ Set(string, string) error }) {
			_ = f.Set("no-ask", "true")
			_ = f.Set("notify-only", "true")
		},
		"ask with decision": func(f interface{ Set(string, string) error }) {
			_ = f.Set("ask", audienceMember)
			_ = f.Set("decision-intent", "true")
		},
		"no-ask with veto": func(f interface{ Set(string, string) error }) {
			_ = f.Set("no-ask", "true")
			_ = f.Set("decision-veto", "true")
		},
	}
	for name, setup := range cases {
		cmd := newIssueCommentAddTestCmd()
		cmd.Flags().String("ask", "", "")
		cmd.Flags().Bool("no-ask", false, "")
		cmd.Flags().Bool("notify-only", false, "")
		cmd.Flags().Bool("decision-intent", false, "")
		cmd.Flags().Bool("decision-record", false, "")
		cmd.Flags().Bool("decision-veto", false, "")
		setup(cmd.Flags())
		_ = cmd.Flags().Set("content", "x")
		err := runIssueCommentAdd(cmd, []string{"11111111-1111-4111-8111-111111111111"})
		if err == nil || !strings.Contains(err.Error(), "--ask") && !strings.Contains(err.Error(), "--no-ask") {
			t.Errorf("%s: err = %v, want a local --ask/--no-ask refusal", name, err)
		}
	}
}

func statusBodyFor(t *testing.T, status, waitingOn string) (map[string]any, error) {
	t.Helper()
	var body map[string]any
	newBlockedOnTestServer(t, true, &body)
	cmd := newIssueStatusTestCmd()
	cmd.Flags().String("waiting-on", "", "")
	if waitingOn != "" {
		if err := cmd.Flags().Set("waiting-on", waitingOn); err != nil {
			t.Fatal(err)
		}
	}
	err := runIssueStatus(cmd, []string{"MUL-2", status})
	return body, err
}

func TestIssueStatusWaitingOnBody(t *testing.T) {
	for in, want := range map[string]string{
		"none":                     "none",
		"member:" + audienceMember: "member:" + audienceMember,
		"  none  ":                 "none",
	} {
		for _, status := range []string{"in_review", "blocked", "need_decision"} {
			body, err := statusBodyFor(t, status, in)
			if err != nil {
				t.Fatalf("%q on %s: %v", in, status, err)
			}
			if body["waiting_on"] != want || body["status"] != status {
				t.Fatalf("%q on %s: body %+v, want waiting_on %q", in, status, body, want)
			}
		}
	}
}

func TestIssueStatusWithoutWaitingOnOmitsIt(t *testing.T) {
	body, err := statusBodyFor(t, "in_review", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, has := body["waiting_on"]; has {
		t.Fatalf("body %+v carries waiting_on", body)
	}
}

func TestIssueStatusWaitingOnRefusals(t *testing.T) {
	cases := map[string][2]string{
		"bare member":        {"member:", "in_review"},
		"member not uuid":    {"member:cheese", "in_review"},
		"unknown form":       {"cheese", "in_review"},
		"blank":              {" ", "in_review"},
		"non waiting status": {"none", "in_progress"},
	}
	for name, c := range cases {
		var body map[string]any
		newBlockedOnTestServer(t, true, &body)
		cmd := newIssueStatusTestCmd()
		cmd.Flags().String("waiting-on", "", "")
		_ = cmd.Flags().Set("waiting-on", c[0])
		err := runIssueStatus(cmd, []string{"MUL-2", c[1]})
		if err == nil || !strings.Contains(err.Error(), "--waiting-on") {
			t.Errorf("%s: err = %v, want a local --waiting-on refusal", name, err)
		}
		if body != nil {
			t.Errorf("%s: a request was sent: %+v", name, body)
		}
	}
}
