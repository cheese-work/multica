package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	cheese = "f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff"
	other  = "11111111-2222-4333-8444-555555555555"
)

type fakeDecision struct {
	Status, Created, Human string
}

// fakeMultica answers `multica decision list` and records streamed calls.
type fakeMultica struct {
	decisions []fakeDecision
	listFails bool
	lists     int
	streamed  []string
}

func (f *fakeMultica) Run(_ context.Context, tool string, args ...string) (Result, error) {
	if tool != "multica" || len(args) < 2 || args[0] != "decision" || args[1] != "list" {
		return Result{Exit: 1, Stderr: "fake multica: unhandled " + strings.Join(args, " ")}, nil
	}
	f.lists++
	if f.listFails {
		return Result{Exit: 1, Stderr: "HTTP 502"}, nil
	}
	rows := []map[string]string{}
	for _, d := range f.decisions {
		rows = append(rows, map[string]string{"status": d.Status, "created_at": d.Created, "human_user_id": d.Human})
	}
	b, _ := json.Marshal(rows)
	return Result{Stdout: string(b)}, nil
}

func (f *fakeMultica) Stream(_ context.Context, tool string, args ...string) (int, error) {
	f.streamed = append(f.streamed, tool+" "+strings.Join(args, " "))
	return 0, nil
}

// wake runs one guarded multica call and returns the exit code, stderr and decision log.
func wake(t *testing.T, f *fakeMultica, args ...string) (int, string, string) {
	t.Helper()
	var out, errb, log bytes.Buffer
	e := &Engine{Policy: DefaultPolicy(), Runner: f, Log: &log, Out: &out, Err: &errb}
	code, err := e.Exec(context.Background(), "multica", args)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return code, errb.String(), log.String()
}

func memberWait(verb, issue, member string) []string {
	return []string{"issue", "wakeup", verb, issue, "--event", "comment.created",
		"--filter-actor-type", "member", "--filter-actor-id", member}
}

func hits(log string) int { return strings.Count(log, `"action":"shadow-hit"`) }

// The CHE-5188 sequence on 12 Nov 2026: both asks were answered, then the agent
// registered a wait on Cheese with no new request.
func TestWaitOnMemberWithNoOpenRequestIsAShadowHit(t *testing.T) {
	answered := []fakeDecision{
		{"answered", "2026-11-12T05:27:25Z", cheese},
		{"answered", "2026-11-12T06:09:49Z", cheese},
	}
	for _, verb := range []string{"create", "update"} {
		f := &fakeMultica{decisions: answered}
		args := memberWait(verb, "CHE-5188", cheese)
		code, errs, log := wake(t, f, args...)
		if code != 0 || len(f.streamed) != 1 {
			t.Fatalf("%s: shadow mode must run the call: exit %d streamed %v", verb, code, f.streamed)
		}
		if errs != "" {
			t.Errorf("%s: shadow mode prints nothing to the agent, got %q", verb, errs)
		}
		if hits(log) != 1 || !strings.Contains(log, `"issue":"CHE-5188"`) || !strings.Contains(log, `"subcommand":"issue wakeup `+verb+`"`) {
			t.Errorf("%s: want one hit with the issue key and command, got %q", verb, log)
		}
	}
}

func TestWaitRightAfterANewOpenRequestIsNoHit(t *testing.T) {
	f := &fakeMultica{decisions: []fakeDecision{
		{"answered", "2026-11-12T06:09:49Z", cheese},
		{"open", "2026-11-12T08:46:00Z", cheese},
	}}
	_, _, log := wake(t, f, memberWait("create", "CHE-5188", cheese)...)
	if hits(log) != 0 {
		t.Errorf("unexpected hit: %q", log)
	}
}

func TestOpenRequestOlderThanNewestAnswerOrForAnotherMemberStillHits(t *testing.T) {
	cases := map[string][]fakeDecision{
		"open request is older than the newest answer": {
			{"open", "2026-11-12T05:00:00Z", cheese}, {"answered", "2026-11-12T06:00:00Z", cheese}},
		"open request is for another member": {
			{"answered", "2026-11-12T06:00:00Z", cheese}, {"open", "2026-11-12T08:00:00Z", other}},
		"no request at all": nil,
	}
	for name, ds := range cases {
		f := &fakeMultica{decisions: ds}
		if _, _, log := wake(t, f, memberWait("create", "CHE-5188", cheese)...); hits(log) != 1 {
			t.Errorf("%s: want one hit, got %q", name, log)
		}
	}
}

func TestWaitsThatAreNotOnAMemberAreNotChecked(t *testing.T) {
	for _, args := range [][]string{
		{"issue", "wakeup", "create", "CHE-5188", "--event", "comment.created", "--filter-actor-type", "agent"},
		{"issue", "wakeup", "create", "CHE-5188", "--until-status", "done"},
		{"issue", "wakeup", "create", "CHE-5188", "--after", "10m"},
		{"issue", "wakeup", "list", "CHE-5188"},
	} {
		f := &fakeMultica{}
		code, errs, log := wake(t, f, args...)
		if code != 0 || len(f.streamed) != 1 || f.lists != 0 || log != "" && hits(log) != 0 || errs != "" {
			t.Errorf("%v: exit %d streamed %v lists %d log %q", args, code, f.streamed, f.lists, log)
		}
	}
}

func TestLookupFailureRunsTheCallSilently(t *testing.T) {
	f := &fakeMultica{listFails: true}
	code, errs, log := wake(t, f, memberWait("create", "CHE-5188", cheese)...)
	if code != 0 || len(f.streamed) != 1 || errs != "" {
		t.Errorf("exit %d streamed %v errs %q", code, f.streamed, errs)
	}
	if !strings.Contains(log, `"action":"plugin-error"`) || hits(log) != 0 {
		t.Errorf("log %q", log)
	}
}

func TestShadowHitFlagForms(t *testing.T) {
	f := &fakeMultica{}
	args := []string{"issue", "wakeup", "create", "CHE-5188", "--event=comment.created",
		"--filter-actor-type=member", fmt.Sprintf("--filter-actor-id=%s", cheese)}
	if _, _, log := wake(t, f, args...); hits(log) != 1 {
		t.Errorf("--flag=value form: %q", log)
	}
}
