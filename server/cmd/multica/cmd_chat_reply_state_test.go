package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// CHE-4150 (L6 of CHE-2769): `multica chat reply-state` posts the agent's turn outcome (T6-T8) to
// POST /api/chat/outcome. The server validates against the DM's open asks; the CLI only refuses
// what is invalid without looking at the DM.

type outcomeCall struct {
	called bool
	body   map[string]any
}

func serveOutcome(t *testing.T, status int, reply string) *outcomeCall {
	t.Helper()
	call := &outcomeCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call.called = true
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat/outcome" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		_ = json.NewDecoder(r.Body).Decode(&call.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	return call
}

func replyState(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command, _, err := chatCmd.Find(append([]string{"reply-state"}, args...))
	if err != nil || command == chatCmd || command.Name() != "reply-state" {
		t.Fatal("chat reply-state command is missing")
	}
	// Flags live on the shared command: reset them, then parse the real argv.
	command.Flags().VisitAll(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
	if err := command.ParseFlags(args); err != nil {
		return "", err
	}
	return captureStdout(t, func() error { return command.RunE(command, command.Flags().Args()) })
}

func TestChatReplyStateQuestionNamesTheAsk(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":["a1"]}`)
	if _, err := replyState(t, "question", "--ask", "a1"); err != nil {
		t.Fatal(err)
	}
	if call.body["kind"] != "question" || call.body["ask"] != "a1" {
		t.Fatalf("body = %v, want kind=question ask=a1", call.body)
	}
	if n, _ := call.body["new"].(bool); n {
		t.Fatalf("body = %v, new must not be set", call.body)
	}
}

func TestChatReplyStateQuestionNewOpensAnAsk(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":["a2"]}`)
	out, err := replyState(t, "question", "--new")
	if err != nil {
		t.Fatal(err)
	}
	if call.body["kind"] != "question" || call.body["new"] != true {
		t.Fatalf("body = %v, want kind=question new=true", call.body)
	}
	if !strings.Contains(out, "a2") {
		t.Fatalf("output %q must name the ask id", out)
	}
}

func TestChatReplyStateQuestionWithoutIDLeavesTheChoiceToTheServer(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":["a1"]}`)
	if _, err := replyState(t, "question"); err != nil {
		t.Fatal(err)
	}
	if call.body["kind"] != "question" || call.body["ask"] != nil || call.body["new"] != nil {
		t.Fatalf("body = %v, want only kind=question", call.body)
	}
}

// A18: two open asks and no id is invalid; the server says so and the CLI fails with its reason.
func TestChatReplyStateQuestionWithTwoOpenAsksIsRefused(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	serveOutcome(t, 400, `{"error":"2 open asks: name one with ask, or pass new"}`)
	_, err := replyState(t, "question")
	if err == nil || !strings.Contains(err.Error(), "2 open asks") {
		t.Fatalf("err = %v, want the server's invalid reason", err)
	}
}

func TestChatReplyStateDoneSendsEveryNamedAsk(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":["a1","a2"]}`)
	if _, err := replyState(t, "done", "--ask", "a1", "--ask", "a2"); err != nil {
		t.Fatal(err)
	}
	ids, _ := call.body["ask_ids"].([]any)
	if call.body["kind"] != "done" || len(ids) != 2 || ids[0] != "a1" || ids[1] != "a2" {
		t.Fatalf("body = %v, want kind=done ask_ids=[a1 a2]", call.body)
	}
}

// A18: done without --ask is invalid, refused before any request.
func TestChatReplyStateDoneWithoutAskIsRefused(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":[]}`)
	_, err := replyState(t, "done")
	if err == nil || !strings.Contains(err.Error(), "--ask") {
		t.Fatalf("err = %v, want a refusal naming --ask", err)
	}
	if call.called {
		t.Fatal("done without --ask must not reach the server")
	}
}

func TestChatReplyStateInfoSendsNoAsk(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":[]}`)
	if _, err := replyState(t, "info"); err != nil {
		t.Fatal(err)
	}
	if call.body["kind"] != "info" || call.body["ask"] != nil || call.body["ask_ids"] != nil {
		t.Fatalf("body = %v, want only kind=info", call.body)
	}
}

func TestChatReplyStateRefusesAmbiguousFlags(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	call := serveOutcome(t, 200, `{"ask_ids":[]}`)
	for _, args := range [][]string{
		{"question", "--ask", "a1", "--new"},
		{"question", "--ask", "a1", "--ask", "a2"},
		{"info", "--ask", "a1"},
		{"info", "--new"},
		{"done", "--new", "--ask", "a1"},
	} {
		if _, err := replyState(t, args...); err == nil {
			t.Errorf("%v must be refused", args)
		}
	}
	if call.called {
		t.Fatal("an ambiguous call must not reach the server")
	}
}
