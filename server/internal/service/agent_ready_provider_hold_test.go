package service

// This file holds the AgentReadiness wiring tests for CHE-588's provider-hold
// check (split out of provider_hold_test.go, which owns the pure-function
// tests for ResolveModelProvider / ProviderHoldsFromSettings /
// ProviderHoldNotice, to keep both files under the repo's ~500 LOC guidance).
// The fake db.DBTX types here follow the scanErrDBTX / runtimeRowsDBTX
// pattern already established in agent_runtime_lookup_test.go, so these tests
// exercise providerHoldVerdict / AgentReadiness end-to-end without requiring
// a live Postgres connection (this package's DB-backed tests otherwise skip
// via sharedTestPool when Postgres is unreachable).

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/dispatch"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// --- AgentReadiness wiring --------------------------------------------

// workspaceAgentsDBTX is a minimal fake DBTX serving exactly the two queries
// providerHoldVerdict issues: GetWorkspace (QueryRow, settings only matters)
// and ListAgents (Query, for substitute listing). It exists because this
// package's DB-backed tests skip without a live Postgres (sharedTestPool),
// and the provider-hold wiring itself is pure logic over rows this fake can
// produce deterministically without one — see agent_runtime_lookup_test.go's
// scanErrDBTX / runtimeRowsDBTX for the established pattern this mirrors.
type workspaceAgentsDBTX struct {
	settings   []byte
	settingsOK bool
	agents     []db.Agent
}

func (d workspaceAgentsDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (d workspaceAgentsDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return &agentRows{rows: d.agents}, nil
}

func (d workspaceAgentsDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	if !d.settingsOK {
		return errRow{err: pgx.ErrNoRows}
	}
	return workspaceRow{settings: d.settings}
}

// workspaceRow implements pgx.Row for GetWorkspace's 13-column Scan. Only
// Settings (position 4) is meaningful to providerHoldVerdict; every other
// destination is left zero-valued, which is safe because AgentReadiness never
// reads them.
type workspaceRow struct{ settings []byte }

func (r workspaceRow) Scan(dest ...any) error {
	if len(dest) > 4 {
		if p, ok := dest[4].(*[]byte); ok {
			*p = r.settings
		}
	}
	return nil
}

// agentRows implements pgx.Rows over a slice of db.Agent for ListAgents's
// 28-column Scan. Positions are ListAgents' own column order (agent.sql.go):
// 0=ID, 2=Name, 15=ArchivedAt, 19=Model — the four fields providerHoldSubstitutes
// and its callers actually read.
type agentRows struct {
	rows []db.Agent
	i    int
}

func (r *agentRows) Close()                                       {}
func (r *agentRows) Err() error                                   { return nil }
func (r *agentRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *agentRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *agentRows) Values() ([]any, error)                       { return nil, nil }
func (r *agentRows) RawValues() [][]byte                          { return nil }
func (r *agentRows) Conn() *pgx.Conn                              { return nil }
func (r *agentRows) TypeMap() *pgtype.Map                         { return nil }

func (r *agentRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *agentRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	for i, d := range dest {
		switch i {
		case 0:
			if p, ok := d.(*pgtype.UUID); ok {
				*p = row.ID
			}
		case 2:
			if p, ok := d.(*string); ok {
				*p = row.Name
			}
		case 15:
			if p, ok := d.(*pgtype.Timestamptz); ok {
				*p = row.ArchivedAt
			}
		case 19:
			if p, ok := d.(*pgtype.Text); ok {
				*p = row.Model
			}
		}
	}
	return nil
}

func uuidFromByte(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{b}, Valid: true}
}

// TestAgentReadinessProviderHoldBlocksHeldProvider is the primary acceptance
// case: an OpenAI-backed agent, under a workspace hold on "openai", must come
// back Blocked with dispatch.ReasonProviderHold — decided entirely inside
// AgentReadiness, before any runtime row is read (this agent has no
// RuntimeID bound at all, which would normally trip
// ReasonAgentRuntimeRequired first; the hold must win instead).
func TestAgentReadinessProviderHoldBlocksHeldProvider(t *testing.T) {
	settings := []byte(`{"provider_holds":[{"provider":"openai","text":"Stop all routing to OpenAI based agents. This constraint has no set end date."}]}`)
	agent := db.Agent{
		ID:          uuidFromByte(1),
		WorkspaceID: uuidFromByte(9),
		Name:        "Nova",
		Model:       pgtype.Text{String: "gpt-5.6-sol", Valid: true},
		// No RuntimeID bound — proves the hold is checked before
		// ReasonAgentRuntimeRequired, not merely before the runtime lookup.
	}
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{settings: settings, settingsOK: true})}

	got, err := AgentReadiness(t.Context(), lookup, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err)
	}
	if !got.Blocked() {
		t.Fatalf("held OpenAI agent: got %+v, want Blocked", got)
	}
	if got.Reason != dispatch.ReasonProviderHold {
		t.Errorf("held OpenAI agent: reason = %q, want %q", got.Reason, dispatch.ReasonProviderHold)
	}
	if !strings.Contains(got.Detail, "Nova") || !strings.Contains(got.Detail, "openai") || !strings.Contains(got.Detail, "Stop all routing to OpenAI based agents") {
		t.Errorf("notice missing agent/provider/hold text: %q", got.Detail)
	}
}

// TestAgentReadinessProviderHoldClaudeUnaffected pins the other half of the
// same acceptance criterion: a hold on "openai" must never touch a
// Claude-backed agent, even in the same workspace with the same hold active.
// A resolver bug that matched providers too broadly would show up here as a
// false Blocked.
func TestAgentReadinessProviderHoldClaudeUnaffected(t *testing.T) {
	settings := []byte(`{"provider_holds":[{"provider":"openai","text":"Stop all routing to OpenAI based agents."}]}`)
	agent := db.Agent{
		ID:          uuidFromByte(2),
		WorkspaceID: uuidFromByte(9),
		Name:        "Mika",
		Model:       pgtype.Text{String: "claude-sonnet-5[1m]", Valid: true},
	}
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{settings: settings, settingsOK: true})}

	got, err := AgentReadiness(t.Context(), lookup, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err)
	}
	// Not runtime-bound either, so the verdict falls through to the ordinary
	// "no runtime bound" case — the point is that it is NOT the provider-hold
	// block.
	if got.Reason == dispatch.ReasonProviderHold {
		t.Fatalf("claude-sonnet-5[1m] under an openai hold: got %+v, want unaffected by the hold", got)
	}
	if got.Reason != dispatch.ReasonAgentRuntimeRequired {
		t.Errorf("got %+v, want the ordinary agent_runtime_required verdict", got)
	}
}

// TestAgentReadinessProviderHoldCustomRoutedModel covers the routing-prefixed
// id from the acceptance criteria: custom:c00-anthropic:claude-sonnet-5 must
// resolve to anthropic and stay unaffected by an openai hold, proving the
// iterative prefix-peel is wired all the way through AgentReadiness, not just
// unit-tested in isolation.
func TestAgentReadinessProviderHoldCustomRoutedModel(t *testing.T) {
	settings := []byte(`{"provider_holds":[{"provider":"openai","text":"hold"}]}`)
	agent := db.Agent{
		ID:          uuidFromByte(3),
		WorkspaceID: uuidFromByte(9),
		Name:        "Kit",
		Model:       pgtype.Text{String: "custom:c00-anthropic:claude-sonnet-5", Valid: true},
	}
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{settings: settings, settingsOK: true})}

	got, _ := AgentReadiness(t.Context(), lookup, agent)
	if got.Reason == dispatch.ReasonProviderHold {
		t.Fatalf("custom-routed anthropic model under an openai hold: got %+v, want unaffected", got)
	}
}

// TestAgentReadinessProviderHoldUnknownModelDoesNotBlock covers the "unknown
// model must not block" acceptance criterion end-to-end: a garbage model
// string, with a hold configured, must fall through exactly as if no hold
// existed rather than refuse admission out of caution.
func TestAgentReadinessProviderHoldUnknownModelDoesNotBlock(t *testing.T) {
	settings := []byte(`{"provider_holds":[{"provider":"openai","text":"hold"}]}`)
	agent := db.Agent{
		ID:          uuidFromByte(4),
		WorkspaceID: uuidFromByte(9),
		Name:        "Ghost",
		Model:       pgtype.Text{String: "totally-unknown-future-model", Valid: true},
	}
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{settings: settings, settingsOK: true})}

	got, err := AgentReadiness(t.Context(), lookup, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err)
	}
	if got.Reason == dispatch.ReasonProviderHold {
		t.Fatalf("unresolvable model: got %+v, want NOT blocked by the hold", got)
	}
}

// TestAgentReadinessProviderHoldFailsClosedOnMalformedSettings is the
// fail-closed acceptance criterion wired all the way through AgentReadiness:
// malformed settings JSON must refuse the agent rather than let it proceed as
// if no hold were configured — the opposite of this codebase's usual
// fail-open settings convention, and deliberately so (see
// ProviderHoldsFromSettings's doc comment).
func TestAgentReadinessProviderHoldFailsClosedOnMalformedSettings(t *testing.T) {
	agent := db.Agent{
		ID:          uuidFromByte(5),
		WorkspaceID: uuidFromByte(9),
		Name:        "Ada",
		Model:       pgtype.Text{String: "gpt-5.6-sol", Valid: true},
	}
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{
		settings:   []byte(`{"provider_holds": not-json`),
		settingsOK: true,
	})}

	got, err := AgentReadiness(t.Context(), lookup, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error (fail-closed must be a Blocked verdict, not a Go error that a caller could swallow): %v", err)
	}
	if !got.Blocked() {
		t.Fatalf("malformed provider-hold settings: got %+v, want Blocked (fail closed)", got)
	}
}

// TestAgentReadinessNoHoldsConfiguredUnaffected is the "no holds configured"
// acceptance criterion: every agent must behave exactly as before this
// feature existed when the workspace has never written provider_holds. Also
// covers the common case where GetWorkspace itself errors (no row, or —
// realistically — most callers hitting this path in production always find
// one): providerHoldVerdict must fall through rather than block.
func TestAgentReadinessNoHoldsConfiguredUnaffected(t *testing.T) {
	agent := db.Agent{
		ID:          uuidFromByte(6),
		WorkspaceID: uuidFromByte(9),
		Name:        "Plain",
		Model:       pgtype.Text{String: "gpt-5.6-sol", Valid: true},
	}
	// No settings at all (empty workspace row) — the overwhelmingly common
	// case for every workspace that has never touched this feature.
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{settingsOK: true})}
	got, err := AgentReadiness(t.Context(), lookup, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err)
	}
	if got.Reason == dispatch.ReasonProviderHold {
		t.Fatalf("no holds configured: got %+v, want unaffected", got)
	}
	if got.Reason != dispatch.ReasonAgentRuntimeRequired {
		t.Errorf("got %+v, want the ordinary agent_runtime_required verdict (unaffected by this feature)", got)
	}

	// GetWorkspace failing outright (no row) must also fall through, not
	// block — there is nothing to fail closed ON.
	lookupNoWorkspace := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{settingsOK: false})}
	got2, err2 := AgentReadiness(t.Context(), lookupNoWorkspace, agent)
	if err2 != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err2)
	}
	if got2.Reason == dispatch.ReasonProviderHold {
		t.Fatalf("no workspace row: got %+v, want unaffected (nothing to fail closed on)", got2)
	}
}

// TestAgentReadinessProviderHoldNilQueriesFallsThrough covers the
// RuntimeLookup{} zero-value case reason_code_test.go's agent-only assertions
// already exercise (no Queries set at all) — providerHoldVerdict must not
// panic on a nil Queries and must simply decline to evaluate a hold.
func TestAgentReadinessProviderHoldNilQueriesFallsThrough(t *testing.T) {
	agent := db.Agent{Model: pgtype.Text{String: "gpt-5.6-sol", Valid: true}}
	got, err := AgentReadiness(t.Context(), RuntimeLookup{}, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err)
	}
	if got.Reason != dispatch.ReasonAgentRuntimeRequired || !got.Blocked() {
		t.Errorf("got %+v, want blocked/agent_runtime_required (unaffected by absent Queries)", got)
	}
}

// TestAgentReadinessProviderHoldListsSubstitutes covers the substitute
// listing: a second, non-held agent in the same workspace must be named in
// the refusal notice.
func TestAgentReadinessProviderHoldListsSubstitutes(t *testing.T) {
	settings := []byte(`{"provider_holds":[{"provider":"openai","text":"hold text"}]}`)
	held := db.Agent{
		ID:          uuidFromByte(7),
		WorkspaceID: uuidFromByte(9),
		Name:        "Nova",
		Model:       pgtype.Text{String: "gpt-5.6-sol", Valid: true},
	}
	substitute := db.Agent{
		ID:    uuidFromByte(8),
		Name:  "Mika",
		Model: pgtype.Text{String: "claude-sonnet-5", Valid: true},
	}
	lookup := RuntimeLookup{Queries: db.New(workspaceAgentsDBTX{
		settings:   settings,
		settingsOK: true,
		agents:     []db.Agent{held, substitute},
	})}

	got, err := AgentReadiness(t.Context(), lookup, held)
	if err != nil {
		t.Fatalf("AgentReadiness: unexpected error: %v", err)
	}
	if !got.Blocked() || got.Reason != dispatch.ReasonProviderHold {
		t.Fatalf("got %+v, want Blocked/provider_hold", got)
	}
	if !strings.Contains(got.Detail, "Mika") {
		t.Errorf("notice omits the non-held substitute: %q", got.Detail)
	}
	// The held agent itself must never list itself as its own substitute.
	if strings.Count(got.Detail, "Nova") != 1 {
		t.Errorf("notice should name Nova once (as the blocked agent), not also as a substitute: %q", got.Detail)
	}
}
