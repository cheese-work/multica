package service

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A write holds one transaction from its revision check to its commit, alias
// effects included. On a pool of one connection, any second connection the
// request opens while that transaction is held can never be served.
func TestSaveWakeupDefinitionUsesOneConnection(t *testing.T) {
	f, owner := newPrincipalFixture(t)
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Skip("no database")
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)
	s := &IssueWakeupService{Tasks: &TaskService{Queries: db.New(one), TxStarter: one}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ref := WakeupScopeRef{Kind: WakeupScopeWorkspace, ID: parseTestUUID(t, f.WorkspaceID), WorkspaceID: parseTestUUID(t, f.WorkspaceID)}
	var patch WakeupConfigPatch
	patch.Enabled = wakeupField[bool]{Set: true, Value: false}
	patch.Instruction = wakeupField[string]{Set: true, Value: "alias text"}
	patch.Name = wakeupField[string]{Set: true, Value: "label"}
	view, err := s.SaveWakeupDefinition(ctx, ref, parseTestUUID(t, owner), WakeupDefinitionWrite{RuleKey: SystemRuleChildDone, Patch: patch})
	if err != nil {
		t.Fatalf("save on a one-connection pool: %v", err)
	}
	if !view.Patch.Enabled.Set || view.Patch.Instruction.Value != "alias text" {
		t.Fatalf("view must carry the alias values: %+v", view.Patch)
	}
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
}

// A list must describe each definition and its resolved target from one
// snapshot. A write that lands between reading the definitions and resolving
// them must not pair the old patch with the new target: visibility is judged on
// the resolved target, so a mismatch shows a private target as visible.
func TestListWakeupDefinitionsIsOneSnapshot(t *testing.T) {
	f, owner := newPrincipalFixture(t)
	s := &IssueWakeupService{Tasks: f.svc.TaskSvc}
	ws := parseTestUUID(t, f.WorkspaceID)
	project := parseTestUUID(t, f.Project(t, "snapshot project"))
	ref := WakeupScopeRef{Kind: WakeupScopeProject, ID: project, WorkspaceID: ws}
	member := parseTestUUID(t, owner)
	agentA, agentB := f.privateAgentOwnedBy(t, owner, "a"), f.privateAgentOwnedBy(t, owner, "b")
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)

	save := func(revision int64, agent, instruction string) {
		t.Helper()
		var p WakeupConfigPatch
		p.Target = wakeupObject{Set: true, Value: []byte(`{"type":"agent","id":"` + agent + `"}`)}
		p.Instruction = wakeupField[string]{Set: true, Value: instruction}
		if _, err := s.SaveWakeupDefinition(context.Background(), ref, member, WakeupDefinitionWrite{RuleKey: SystemRulePRMerged, Revision: revision, Patch: p}); err != nil {
			t.Fatal(err)
		}
	}
	save(0, agentA, "secret prompt")
	afterWakeupListRows = func() { save(1, agentB, "public prompt") }
	defer func() { afterWakeupListRows = nil }()

	views, err := s.ListWakeupDefinitions(context.Background(), ref)
	if err != nil || len(views) != 1 {
		t.Fatalf("list = %v, %v", views, err)
	}
	v := views[0]
	if v.Effective == nil {
		t.Fatal("no resolution")
	}
	if string(v.Patch.Target.Value) != string(v.Effective.Target.Value) || v.Patch.Instruction.Value != v.Effective.Instruction.Value {
		t.Fatalf("the list paired revision %d's patch (%s, %q) with a resolution of another revision (%s, %q)",
			v.Revision, v.Patch.Target.Value, v.Patch.Instruction.Value, v.Effective.Target.Value, v.Effective.Instruction.Value)
	}
}
