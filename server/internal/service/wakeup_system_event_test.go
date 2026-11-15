package service

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// D1 of .plans/CHE-5696-system-events-wakeup.md: where a system event type may
// be used.
func TestValidateSystemWakeupEvents(t *testing.T) {
	sha := map[string]string{"sha": "abc"}
	for _, tc := range []struct {
		name   string
		events []string
		scope  WakeupScope
		target string
		match  map[string]string
		ok     bool
	}{
		{"issue facts need nothing", []string{"comment.created"}, WakeupScopeWorkspace, "agent", nil, true},
		{"runtime.online at issue scope", []string{"runtime.online"}, WakeupScopeIssue, "agent", nil, true},
		{"runtime.offline at issue scope, assignee", []string{"runtime.offline"}, WakeupScopeIssue, "assignee", nil, true},
		{"autopilot.skipped at issue scope", []string{"autopilot.skipped"}, WakeupScopeIssue, "agent", nil, true},
		{"mixed system and issue facts at issue scope", []string{"runtime.online", "comment.created"}, WakeupScopeIssue, "agent", nil, true},
		{"runtime.online at workspace, agent target", []string{"runtime.online"}, WakeupScopeWorkspace, "agent", nil, false},
		{"runtime.online at project, squad target", []string{"runtime.online"}, WakeupScopeProject, "squad", nil, false},
		{"runtime.online at workspace, assignee target", []string{"runtime.online"}, WakeupScopeWorkspace, "assignee", nil, false},
		{"autopilot.skipped at workspace, autopilot target", []string{"autopilot.skipped"}, WakeupScopeWorkspace, "autopilot", nil, true},
		{"autopilot.failed at project, autopilot target", []string{"autopilot.failed"}, WakeupScopeProject, "autopilot", nil, true},
		{"server.deployed without match", []string{"server.deployed"}, WakeupScopeIssue, "agent", nil, false},
		{"server.deployed, match without sha or digest", []string{"server.deployed"}, WakeupScopeIssue, "agent", map[string]string{"host": "x"}, false},
		{"server.deployed, empty sha", []string{"server.deployed"}, WakeupScopeIssue, "agent", map[string]string{"sha": ""}, false},
		{"server.deployed, sha", []string{"server.deployed"}, WakeupScopeIssue, "agent", sha, true},
		{"server.deployed, digest", []string{"server.deployed"}, WakeupScopeIssue, "agent", map[string]string{"digest": "sha256:1"}, true},
		{"server.deployed among others still needs match", []string{"runtime.online", "server.deployed"}, WakeupScopeIssue, "agent", nil, false},
		{"daemon.restarted is reserved", []string{"daemon.restarted"}, WakeupScopeIssue, "agent", nil, false},
		{"unknown system-looking type", []string{"runtime.sideways"}, WakeupScopeIssue, "agent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSystemWakeupEvents(tc.events, tc.scope, tc.target, tc.match)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.ok && !errors.Is(err, ErrWakeupInput):
				t.Fatalf("err = %v, want an input error", err)
			}
		})
	}
}

// The definition path: the scope that supplies the trigger decides.
func TestValidateEffectiveWakeupSystemEvents(t *testing.T) {
	eff := func(t *testing.T, scope WakeupScope, fields string) EffectiveWakeupConfig {
		t.Helper()
		def := &WakeupDefinition{Scope: scope, Revision: 1, Patch: patchOf(t, fields)}
		in := WakeupResolveInput{RuleKey: "11111111-1111-4111-8111-111111111111"}
		root := &WakeupDefinition{Scope: WakeupScopeWorkspace, Revision: 1, Root: true, Patch: patchOf(t, `"enabled":true,"trigger":{"kind":"event","events":["comment.created"]},"instruction":"x"`)}
		in.Workspace = root
		switch scope {
		case WakeupScopeWorkspace:
			def.Root = true
			in.Workspace = def
		case WakeupScopeProject:
			def.ScopeID = pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
			in.IssueProjectID, in.Project = def.ScopeID, def
		default:
			in.Issue = def
		}
		e, err := ResolveWakeupConfig(in)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	const online = `"enabled":true,"trigger":{"kind":"event","events":["runtime.online"]},"instruction":"x"`
	if err := validateEffectiveWakeup(eff(t, WakeupScopeIssue, online)); err != nil {
		t.Fatalf("issue scope refused: %v", err)
	}
	for _, scope := range []WakeupScope{WakeupScopeWorkspace, WakeupScopeProject} {
		if err := validateEffectiveWakeup(eff(t, scope, online)); !errors.Is(err, ErrWakeupInput) {
			t.Fatalf("%s scope agent target = %v, want an input error", scope, err)
		}
	}
}

// The per-issue waiter API accepts system types; server.deployed has no match
// field yet (S3), so it is refused rather than stored unfiltered.
func TestIssueWakeupValidateSystemEvents(t *testing.T) {
	s := IssueWakeupService{}
	for _, tc := range []struct {
		events []string
		mod    func(*WakeupInput)
		ok     bool
	}{
		{[]string{"runtime.online"}, nil, true},
		{[]string{"runtime.offline", "comment.created"}, nil, true},
		{[]string{"server.deployed"}, nil, false},
		{[]string{"daemon.restarted"}, nil, false},
		{[]string{"runtime.online"}, func(in *WakeupInput) { in.FilterActorType, in.FilterActorID = "member", "u" }, false},
		{[]string{"runtime.online"}, func(in *WakeupInput) { in.FilterAgentID = "a" }, false},
		{[]string{"runtime.online"}, func(in *WakeupInput) { in.FilterTaskID = "t" }, false},
	} {
		in := WakeupInput{Kind: "event", Instruction: "x", EventTypes: tc.events}
		if tc.mod != nil {
			tc.mod(&in)
		}
		_, err := s.Validate(&in, time.Now())
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrWakeupInput)) {
			t.Errorf("%v: err = %v, ok want %v", tc.events, err, tc.ok)
		}
	}
}
