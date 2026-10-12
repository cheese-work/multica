package service

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func insertDefinition(t *testing.T, f principalFixture, scopeKind, scopeID, key string, root bool, config string) error {
	t.Helper()
	_, err := f.Pool.Exec(context.Background(),
		`INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config) VALUES($1,$2,$3,$4,$5,$6::jsonb)`,
		f.WorkspaceID, scopeKind, scopeID, key, root, config)
	return err
}

func constraintOf(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.ConstraintName
	}
	return ""
}

func TestWakeupDefinitionSchemaIdentityAndChecks(t *testing.T) {
	f, _, issue, _ := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	issueID := util.UUIDToString(issue)

	if err := insertDefinition(t, f, "workspace", f.WorkspaceID, "child_done", false, `{"v":1,"enabled":false}`); err != nil {
		t.Fatalf("workspace override: %v", err)
	}
	if err := insertDefinition(t, f, "workspace", f.WorkspaceID, "child_done", false, `{"v":1}`); constraintOf(err) != "issue_wakeup_definition_identity_idx" {
		t.Fatalf("duplicate (workspace, scope, key) must violate the identity index, got %v", err)
	}
	// Same key at another scope is a different identity.
	if err := insertDefinition(t, f, "issue", issueID, "child_done", false, `{"v":1}`); err != nil {
		t.Fatalf("issue override of same key: %v", err)
	}
	custom := "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10"
	if err := insertDefinition(t, f, "workspace", f.WorkspaceID, custom, true, `{"v":1,"enabled":true}`); err != nil {
		t.Fatalf("custom workspace root: %v", err)
	}

	bad := []struct {
		name, constraint string
		run              func() error
	}{
		{"unknown scope kind", "issue_wakeup_definition_scope_kind_check", func() error {
			return insertDefinition(t, f, "team", issueID, custom, false, `{"v":1}`)
		}},
		{"free-form rule key", "issue_wakeup_definition_rule_key_check", func() error {
			return insertDefinition(t, f, "issue", issueID, "my rule", false, `{"v":1}`)
		}},
		{"workspace scope id must be the workspace", "issue_wakeup_definition_workspace_scope_check", func() error {
			return insertDefinition(t, f, "workspace", issueID, "pr_merged", false, `{"v":1}`)
		}},
		{"built-in key cannot be a root", "issue_wakeup_definition_root_check", func() error {
			return insertDefinition(t, f, "workspace", f.WorkspaceID, "pr_merged", true, `{"v":1}`)
		}},
		{"issue scope cannot be a root", "issue_wakeup_definition_root_check", func() error {
			return insertDefinition(t, f, "issue", issueID, "00000000-0000-4000-8000-000000000001", true, `{"v":1}`)
		}},
		{"config must be a versioned object", "issue_wakeup_definition_config_check", func() error {
			return insertDefinition(t, f, "issue", issueID, "pr_merged", false, `{"enabled":true}`)
		}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if got := constraintOf(c.run()); got != c.constraint {
				t.Fatalf("constraint = %q, want %q", got, c.constraint)
			}
		})
	}
}

func TestWakeupDefinitionRowDecodesIntoResolverInput(t *testing.T) {
	f, _, _, _ := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	if err := insertDefinition(t, f, "workspace", f.WorkspaceID, "child_done", false, `{"v":1,"instruction":"from row","aggregate_limit":9}`); err != nil {
		t.Fatal(err)
	}
	var row db.IssueWakeupDefinition
	err := f.Pool.QueryRow(context.Background(),
		`SELECT workspace_id,scope_kind,scope_id,rule_key,root,config,revision,created_by,updated_by,created_at,updated_at FROM issue_wakeup_definition WHERE workspace_id=$1`,
		f.WorkspaceID).Scan(&row.WorkspaceID, &row.ScopeKind, &row.ScopeID, &row.RuleKey, &row.Root, &row.Config, &row.Revision,
		&row.CreatedBy, &row.UpdatedBy, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if row.Revision != 1 {
		t.Fatalf("new definition revision = %d, want 1", row.Revision)
	}
	def, err := WakeupDefinitionFromRow(row)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveWakeupConfig(WakeupResolveInput{RuleKey: row.RuleKey, Workspace: &def})
	if err != nil {
		t.Fatal(err)
	}
	if got.Config.Instruction.Value != "from row" || len(got.AggregateCaps) != 1 || got.AggregateCaps[0].Limit != 9 {
		t.Fatalf("row did not resolve: %+v", got)
	}

	// A row whose config this build cannot read fails closed.
	if _, err := WakeupDefinitionFromRow(db.IssueWakeupDefinition{ScopeKind: "workspace", RuleKey: "child_done", Config: []byte(`{"v":99}`)}); err == nil {
		t.Fatal("unreadable config must not decode")
	}
	if _, err := WakeupDefinitionFromRow(db.IssueWakeupDefinition{ScopeKind: "galaxy", RuleKey: "child_done", Config: []byte(`{"v":1}`)}); err == nil {
		t.Fatal("unknown scope kind must not decode")
	}
}

func TestIssueWakeupInstanceOriginMetadataIsAdditive(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "local"})
	if w.DefaultRuleKey.Valid || w.DefaultScopeKind.Valid || w.DefaultScopeID.Valid || w.ConfigFingerprint.Valid {
		t.Fatalf("a local wakeup must carry no default-origin metadata: %+v", w)
	}
	exec := func(sql string, args ...any) error {
		_, err := f.Pool.Exec(context.Background(), sql, args...)
		return err
	}
	scope := f.WorkspaceID
	if err := exec(`UPDATE issue_wakeup SET default_rule_key='pr_merged',default_scope_kind='workspace',default_scope_id=$2,config_fingerprint='abc' WHERE id=$1`, w.ID, scope); err != nil {
		t.Fatalf("full origin: %v", err)
	}
	for _, c := range []struct{ name, sql, constraint string }{
		{"key without kind", `UPDATE issue_wakeup SET default_rule_key='x',default_scope_kind=NULL,default_scope_id=NULL WHERE id=$1`, "issue_wakeup_default_origin_check"},
		{"scope without key", `UPDATE issue_wakeup SET default_rule_key=NULL,default_scope_kind='project',default_scope_id=issue_id WHERE id=$1`, "issue_wakeup_default_origin_check"},
		{"issue as origin", `UPDATE issue_wakeup SET default_rule_key='x',default_scope_kind='issue',default_scope_id=issue_id WHERE id=$1`, "issue_wakeup_default_scope_kind_check"},
	} {
		if got := constraintOf(exec(c.sql, w.ID)); got != c.constraint {
			t.Fatalf("%s: constraint = %q, want %q", c.name, got, c.constraint)
		}
	}
}
