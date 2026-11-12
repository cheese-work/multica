package bgagents

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	a = "11111111-1111-1111-1111-111111111111"
	b = "22222222-2222-2222-2222-222222222222"
	c = "33333333-3333-3333-3333-333333333333"
)

func TestCatalog(t *testing.T) {
	tests := []struct {
		job        Job
		runner     RunnerKind
		claim, run time.Duration
		userScoped bool
	}{
		{DMTitle, OutputOnly, 10 * time.Second, 60 * time.Second, true},
		{QuickActions, OutputOnly, 5 * time.Second, 20 * time.Second, true},
		{Compact, ChatCompaction, 30 * time.Second, 0, true}, // run: existing compaction limits
		{Routing, WorkflowStep, 0, 0, false},                 // PR #989 rules
	}
	if len(Catalog) != len(tests) {
		t.Fatalf("catalog has %d jobs, want %d", len(Catalog), len(tests))
	}
	for _, tt := range tests {
		got, ok := SpecFor(tt.job)
		if !ok || got.Runner != tt.runner || got.ClaimDeadline != tt.claim || got.RunDeadline != tt.run || got.UserScoped != tt.userScoped {
			t.Errorf("%s: got %+v ok=%v", tt.job, got, ok)
		}
	}
	if _, ok := SpecFor("nope"); ok {
		t.Error("unknown job must not resolve")
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		raw   string
		code  string // "" = valid
	}{
		{"empty object", WorkspaceScope, `{}`, ""},
		{"valid workspace", WorkspaceScope, `{"dm_title":{"agent_id":"` + a + `","fallback_agent_id":null},"routing":{"agent_id":"` + a + `","statuses":["todo"],"overrides":{"todo":"` + b + `"}}}`, ""},
		{"valid user", UserScope, `{"compact":{"agent_id":"` + a + `","fallback_agent_id":"` + b + `"}}`, ""},
		{"unknown job", WorkspaceScope, `{"nope":{}}`, CodeUnknownJob},
		{"third slot", WorkspaceScope, `{"dm_title":{"agent_id":"` + a + `","tertiary_agent_id":"` + c + `"}}`, CodeUnknownField},
		{"routing extras on dm_title", WorkspaceScope, `{"dm_title":{"statuses":["todo"]}}`, CodeUnknownField},
		{"routing key in user scope", UserScope, `{"routing":{"agent_id":"` + a + `"}}`, CodeRoutingUserScope},
		{"bad agent id", WorkspaceScope, `{"dm_title":{"agent_id":"not-a-uuid"}}`, CodeInvalidAgentID},
		{"bad override id", WorkspaceScope, `{"routing":{"overrides":{"todo":"x"}}}`, CodeInvalidAgentID},
		{"statuses not strings", WorkspaceScope, `{"routing":{"statuses":[1]}}`, CodeInvalidShape},
		{"not an object", WorkspaceScope, `[]`, CodeInvalidShape},
		{"chain not an object", WorkspaceScope, `{"dm_title":"x"}`, CodeInvalidShape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.raw), tt.scope)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				_ = cfg
				return
			}
			var ve *Error
			if !errors.As(err, &ve) || ve.Code != tt.code {
				t.Fatalf("got %v, want code %s", err, tt.code)
			}
		})
	}
	cfg, _ := Parse([]byte(`{"dm_title":{"agent_id":"`+a+`","fallback_agent_id":"`+b+`"}}`), WorkspaceScope)
	if got := cfg[DMTitle]; got.AgentID != a || got.FallbackAgentID != b {
		t.Errorf("parsed chain = %+v", got)
	}
}

func TestResolve(t *testing.T) {
	all := func(Source, string) bool { return true }
	except := func(bad string) func(Source, string) bool {
		return func(_ Source, id string) bool { return id != bad }
	}
	ws := Config{DMTitle: {AgentID: a, FallbackAgentID: b}}
	user := Config{DMTitle: {AgentID: c}}
	tests := []struct {
		name     string
		user, ws Config
		ok       func(Source, string) bool
		want     Resolution
	}{
		{"user wins", user, ws, all, Resolution{UserSource, []Candidate{{Primary, c}}}},
		{"workspace when no user value", nil, ws, all, Resolution{WorkspaceSource, []Candidate{{Primary, a}, {Fallback, b}}}},
		{"workspace when user agent unset", Config{DMTitle: {}}, ws, all, Resolution{WorkspaceSource, []Candidate{{Primary, a}, {Fallback, b}}}},
		{"no agent anywhere", nil, nil, all, Resolution{Source: NoAgent}},
		{"workspace primary ineligible -> fallback", nil, ws, except(a), Resolution{WorkspaceSource, []Candidate{{Fallback, b}}}},
		{"failed user chain does not fall through", user, ws, except(c), Resolution{Source: UserSource}},
		{"workspace both ineligible -> empty", nil, ws, func(Source, string) bool { return false }, Resolution{Source: WorkspaceSource}},
		{"fallback without primary is ignored", nil, Config{DMTitle: {FallbackAgentID: b}}, all, Resolution{Source: NoAgent}},
		{"routing ignores user chain", Config{Routing: {AgentID: c}}, Config{Routing: {AgentID: a}}, all, Resolution{WorkspaceSource, []Candidate{{Primary, a}}}},
		{"unknown job", user, ws, all, Resolution{Source: NoAgent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := DMTitle
			if tt.name == "routing ignores user chain" {
				job = Routing
			}
			if tt.name == "unknown job" {
				job = "nope"
			}
			got := Resolve(job, tt.user, tt.ws, tt.ok)
			if got.Source != tt.want.Source || len(got.Chain) != len(tt.want.Chain) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got.Chain {
				if got.Chain[i] != tt.want.Chain[i] {
					t.Fatalf("got %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestWorkspaceEligible(t *testing.T) {
	ok := db.Agent{Visibility: "workspace", RuntimeID: pgtype.UUID{Valid: true}}
	tests := []struct {
		name string
		mod  func(*db.Agent)
		want bool
	}{
		{"visible, live, on a runtime", func(*db.Agent) {}, true},
		{"private", func(x *db.Agent) { x.Visibility = "private" }, false},
		{"archived", func(x *db.Agent) { x.ArchivedAt = pgtype.Timestamptz{Valid: true} }, false},
		{"no runtime", func(x *db.Agent) { x.RuntimeID = pgtype.UUID{} }, false},
	}
	for _, tt := range tests {
		x := ok
		tt.mod(&x)
		if got := WorkspaceEligible(x); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}

// CHE-4101: an output_only job skips an agent whose runtime provider can read
// files or workspace content, like any other ineligible agent.
func TestResolveSkipsAgentOnIneligibleProvider(t *testing.T) {
	providers := map[string]string{a: "codex", b: "claude"}
	agent := db.Agent{Visibility: "workspace", RuntimeID: pgtype.UUID{Valid: true}}
	eligible := func(_ Source, id string) bool { return Eligible(DMTitle, agent, providers[id]) }

	got := Resolve(DMTitle, nil, Config{DMTitle: {AgentID: a, FallbackAgentID: b}}, eligible)
	if len(got.Chain) != 1 || got.Chain[0] != (Candidate{Fallback, b}) {
		t.Fatalf("want only the claude fallback, got %+v", got)
	}
	got = Resolve(DMTitle, nil, Config{DMTitle: {AgentID: a}}, eligible)
	if len(got.Chain) != 0 {
		t.Fatalf("a lone codex agent must resolve to the no-agent path, got %+v", got)
	}
	// Routing is not an output_only job: the provider gate does not apply.
	if !Eligible(Routing, agent, "codex") {
		t.Fatal("routing must not be gated on provider")
	}
}
