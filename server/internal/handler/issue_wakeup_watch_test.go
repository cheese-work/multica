package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestIssueWakeupWatchInputsRoundTrip(t *testing.T) {
	f := newAskGuardWakeupFixture(t)
	thread := "thread:" + uuid.NewString()
	want := []string{"issue", "pr", "children", thread}
	for _, kind := range []map[string]any{
		{"kind": "at", "after_seconds": 600},
		{"kind": "every", "interval_seconds": 600},
		{"kind": "cron", "cron_expression": "0 * * * *"},
	} {
		body := map[string]any{"instruction": "check", "watch_inputs": want}
		for k, v := range kind {
			body[k] = v
		}
		var created map[string]any
		f.create(t, body).Want(http.StatusCreated).JSON(&created)
		got, _ := created["watch_inputs"].([]any)
		if len(got) != len(want) {
			t.Fatalf("%v create watch_inputs = %v, want %v", kind["kind"], created["watch_inputs"], want)
		}
		var list []map[string]any
		testutil.Call(t, testHandler.ListIssueWakeups, f.wakeupReq(http.MethodGet, "", nil)).Want(http.StatusOK).JSON(&list)
		found := false
		for _, w := range list {
			if w["id"] == created["id"] {
				found = true
				if g, _ := w["watch_inputs"].([]any); len(g) != len(want) {
					t.Fatalf("list watch_inputs = %v, want %v", w["watch_inputs"], want)
				}
			}
		}
		if !found {
			t.Fatal("created wakeup missing from list")
		}
	}
}

func TestIssueWakeupWatchInputsRefused(t *testing.T) {
	f := newAskGuardWakeupFixture(t)
	for name, body := range map[string]map[string]any{
		"unknown value":  {"kind": "at", "after_seconds": 600, "watch_inputs": []string{"labels"}},
		"bad thread id":  {"kind": "at", "after_seconds": 600, "watch_inputs": []string{"thread:nope"}},
		"event rule":     {"kind": "event", "event_types": []string{"comment.created"}, "watch_inputs": []string{"issue"}},
		"condition rule": {"kind": "event", "condition": map[string]any{"type": "pull_request", "event": "conflicts"}, "watch_inputs": []string{"pr"}},
	} {
		t.Run(name, func(t *testing.T) {
			body["instruction"] = "check"
			res := f.create(t, body).Want(http.StatusUnprocessableEntity)
			msg, _ := res.Map()["error"].(string)
			for _, accepted := range []string{"issue", "pr", "children", "thread:<comment-uuid>"} {
				if !strings.Contains(msg, accepted) {
					t.Errorf("422 error lacks %q: %s", accepted, msg)
				}
			}
			if n := f.wakeups(t); n != 0 {
				t.Fatalf("refused wakeup was saved: %d", n)
			}
		})
	}
}

func TestIssueWakeupWatchInputsUpdateBumpsRevision(t *testing.T) {
	f := newAskGuardWakeupFixture(t)
	var created map[string]any
	f.create(t, map[string]any{"kind": "at", "after_seconds": 600, "instruction": "check"}).Want(http.StatusCreated).JSON(&created)
	if w, _ := created["watch_inputs"].([]any); created["watch_inputs"] == nil || len(w) != 0 {
		t.Fatalf("default watch_inputs = %v, want empty array", created["watch_inputs"])
	}
	id := created["id"].(string)
	var updated map[string]any
	f.update(t, id, map[string]any{"kind": "at", "after_seconds": 600, "instruction": "check", "watch_inputs": []string{"pr"}}).Want(http.StatusOK).JSON(&updated)
	if updated["revision"].(float64) <= created["revision"].(float64) {
		t.Fatalf("revision %v -> %v, want bump", created["revision"], updated["revision"])
	}
	if w, _ := updated["watch_inputs"].([]any); len(w) != 1 || w[0] != "pr" {
		t.Fatalf("updated watch_inputs = %v", updated["watch_inputs"])
	}
}
