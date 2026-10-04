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
