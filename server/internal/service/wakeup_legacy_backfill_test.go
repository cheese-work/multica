package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func customizeChildDone(t *testing.T, s *IssueWakeupService, issue pgtype.UUID, enabled *bool, instruction *string) db.IssueWakeup {
	t.Helper()
	w, err := s.UpdateChildDoneRule(context.Background(), issue, SystemWakeupInput{Enabled: enabled, Instruction: instruction})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestLegacyIssueRowProjection(t *testing.T) {
	issue := parseTestUUIDRaw("11111111-1111-4111-8111-111111111111")
	row := func(mut func(*db.IssueWakeup)) db.IssueWakeup {
		w := db.IssueWakeup{IssueID: issue, Enabled: true, SystemRule: systemRuleText(SystemRuleChildDone)}
		mut(&w)
		return w
	}
	customized := pgtype.Timestamptz{Valid: true}
	reason := pgtype.Text{String: wakeupPausedLoop, Valid: true}

	for _, tc := range []struct {
		name         string
		w            db.IssueWakeup
		wantNil      bool
		wantEnabled  *bool
		wantInstruct string
	}{
		{"uncustomized follows inheritance", row(func(w *db.IssueWakeup) { w.Instruction = "stale" }), true, nil, ""},
		{"local wakeup is not a platform rule", row(func(w *db.IssueWakeup) { w.SystemRule = pgtype.Text{}; w.CustomizedAt = customized }), true, nil, ""},
		{"customized off", row(func(w *db.IssueWakeup) { w.CustomizedAt = customized; w.Enabled = false }), false, ptr(false), ""},
		{"customized on with text", row(func(w *db.IssueWakeup) { w.CustomizedAt = customized; w.Instruction = " fix it " }), false, ptr(true), "fix it"},
		{"empty instruction means inherit", row(func(w *db.IssueWakeup) { w.CustomizedAt = customized; w.Instruction = "  " }), false, ptr(true), ""},
		{"loop pause is not an override", row(func(w *db.IssueWakeup) {
			w.CustomizedAt = customized
			w.Enabled = false
			w.PausedReason = reason
			w.Instruction = "keep"
		}), false, nil, "keep"},
		{"blocked runs are not an override", row(func(w *db.IssueWakeup) {
			w.CustomizedAt = customized
			w.Enabled = false
			w.DisabledAt = pgtype.Timestamptz{Valid: true}
		}), true, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, err := LegacyIssueWakeupDefinition(tc.w)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantNil {
				if def != nil {
					t.Fatalf("got %+v, want nil", def)
				}
				return
			}
			if def == nil || def.Scope != WakeupScopeIssue || def.ScopeID != issue || def.Root || def.Revision < 1 {
				t.Fatalf("bad definition: %+v", def)
			}
			if def.Patch.Enabled.Set != (tc.wantEnabled != nil) || (tc.wantEnabled != nil && def.Patch.Enabled.Value != *tc.wantEnabled) {
				t.Fatalf("enabled = %+v, want %v", def.Patch.Enabled, tc.wantEnabled)
			}
			if def.Patch.Instruction.Value != tc.wantInstruct || def.Patch.Instruction.Set != (tc.wantInstruct != "") {
				t.Fatalf("instruction = %+v, want %q", def.Patch.Instruction, tc.wantInstruct)
			}
		})
	}

	a, _ := LegacyIssueWakeupDefinition(row(func(w *db.IssueWakeup) { w.CustomizedAt = customized; w.Instruction = "a" }))
	b, _ := LegacyIssueWakeupDefinition(row(func(w *db.IssueWakeup) { w.CustomizedAt = customized; w.Instruction = "b" }))
	a2, _ := LegacyIssueWakeupDefinition(row(func(w *db.IssueWakeup) { w.CustomizedAt = customized; w.Instruction = "a" }))
	if a.Revision == b.Revision || a.Revision != a2.Revision {
		t.Fatalf("revision must follow content: %d %d %d", a.Revision, b.Revision, a2.Revision)
	}
	// The projection resolves like the legacy row: issue override beats the workspace.
	got, err := ResolveWakeupConfig(WakeupResolveInput{RuleKey: SystemRuleChildDone, Workspace: aliasDef(t, `{"system_wakeup_child_done":false}`, SystemRuleChildDone), Issue: b})
	if err != nil || !got.Enabled() || got.Config.Instruction.Value != "b" {
		t.Fatalf("resolved = %+v %v", got, err)
	}
}

func ptr[T any](v T) *T { return &v }

func parseTestUUIDRaw(s string) pgtype.UUID {
	var id pgtype.UUID
	_ = id.Scan(s)
	return id
}

type backfillSnapshot struct{ tasks, receipts, wakeups, state int }

func snapshotBackfill(t *testing.T, f principalFixture, issue pgtype.UUID) backfillSnapshot {
	t.Helper()
	return backfillSnapshot{
		tasks:    f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issue),
		receipts: f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id=$1)`, issue),
		wakeups:  f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id=$1`, issue),
		state:    f.Count(t, `SELECT coalesce(sum(revision+extract(epoch from updated_at)::bigint),0) FROM issue_wakeup WHERE issue_id=$1`, issue),
	}
}

func TestLegacyBackfillIsIdempotentRestartableAndQueuesNothing(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	ctx := context.Background()
	ws := parseTestUUID(t, f.WorkspaceID)
	assignPRWakeupIssue(t, f, issue, agent)

	off, text := false, "  custom text "
	customizeChildDone(t, s, issue, &off, &text)
	// A second parent whose rule was never customized, and PR rows, are not backfilled.
	other := parseTestUUID(t, f.Issue(t, "uncustomized parent"))
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", other)
	if _, err := s.UpdateChildDoneRule(ctx, other, SystemWakeupInput{}); err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `UPDATE issue_wakeup SET customized_at=NULL WHERE issue_id=$1`, other)
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{Rule: SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 9, MergeCommit: "m"}); err != nil {
		t.Fatal(err)
	}
	// A paused customized rule: the pause is execution state, the text survives.
	paused := parseTestUUID(t, f.Issue(t, "paused parent"))
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", paused)
	on, ptext := true, "paused text"
	customizeChildDone(t, s, paused, &on, &ptext)
	f.Exec(t, `UPDATE issue_wakeup SET enabled=false,paused_reason=$2,disabled_at=now() WHERE issue_id=$1`, paused, wakeupPausedLoop)

	before := []backfillSnapshot{snapshotBackfill(t, f, issue), snapshotBackfill(t, f, other), snapshotBackfill(t, f, paused)}
	issues := []pgtype.UUID{issue, other, paused}

	// Interrupted after one page: only that page's definitions exist.
	first, err := s.BackfillLegacyWakeupDefinitions(ctx, LegacyBackfillRequest{WorkspaceID: ws, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Scanned != 1 || first.Inserted != 1 || first.Done {
		t.Fatalf("first page = %+v", first)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); got != 1 {
		t.Fatalf("definitions after an interrupted run = %d, want 1", got)
	}
	// Restart from the beginning: the finished page is skipped, not duplicated.
	total, err := s.RunLegacyWakeupBackfill(ctx, ws, 1)
	if err != nil {
		t.Fatal(err)
	}
	if total.Inserted != 1 || !total.Done {
		t.Fatalf("restarted run = %+v, want one more insert", total)
	}
	again, err := s.RunLegacyWakeupBackfill(ctx, ws, 50)
	if err != nil || again.Inserted != 0 || again.Scanned != 2 {
		t.Fatalf("second full run = %+v %v, want 2 scanned, 0 inserted", again, err)
	}

	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); got != 2 {
		t.Fatalf("definitions = %d, want 2 (customized child_done rows only)", got)
	}
	if got := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1 AND scope_kind<>'issue'`, f.WorkspaceID); got != 0 {
		t.Fatalf("backfill wrote %d workspace/project definitions; workspace values stay in settings", got)
	}
	var cfg string
	if err := f.Pool.QueryRow(ctx, `SELECT config::text FROM issue_wakeup_definition WHERE scope_id=$1 AND rule_key='child_done'`, issue).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, `"enabled": false`) || !strings.Contains(cfg, `"instruction": "custom text"`) || !strings.Contains(cfg, `"v": 1`) {
		t.Fatalf("definition config = %s", cfg)
	}
	if err := f.Pool.QueryRow(ctx, `SELECT config::text FROM issue_wakeup_definition WHERE scope_id=$1`, paused).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "enabled") || !strings.Contains(cfg, "paused text") {
		t.Fatalf("paused row definition = %s; a pause must not become an override", cfg)
	}
	for i, id := range issues {
		if after := snapshotBackfill(t, f, id); after != before[i] {
			t.Fatalf("backfill changed runtime state of issue %d: %+v -> %+v", i, before[i], after)
		}
	}
}

func TestLegacyBackfillNeverOverwritesExistingOrUnknownDefinition(t *testing.T) {
	f, s, issue, _ := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM issue_wakeup_definition WHERE workspace_id=$1", f.WorkspaceID)
	ctx := context.Background()
	off := false
	customizeChildDone(t, s, issue, &off, nil)
	// A definition from a later version that this build cannot read.
	future := `{"v":99,"enabled":true,"trigger":{"type":"cron"},"unknown_future_field":[1]}`
	if err := insertDefinition(t, f, "issue", uuidString(issue), SystemRuleChildDone, false, future); err != nil {
		t.Fatal(err)
	}
	res, err := s.RunLegacyWakeupBackfill(ctx, parseTestUUID(t, f.WorkspaceID), 10)
	if err != nil || res.Inserted != 0 {
		t.Fatalf("backfill over an existing definition = %+v %v", res, err)
	}
	var cfg string
	if err := f.Pool.QueryRow(ctx, `SELECT config::text FROM issue_wakeup_definition WHERE scope_id=$1 AND rule_key='child_done'`, issue).Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg, `"v": 99`) || !strings.Contains(cfg, "unknown_future_field") {
		t.Fatalf("unknown later configuration was altered: %s", cfg)
	}
	var revision int64
	if err := f.Pool.QueryRow(ctx, `SELECT revision FROM issue_wakeup_definition WHERE scope_id=$1 AND rule_key='child_done'`, issue).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("revision = %d %v", revision, err)
	}
}

func TestLegacyBackfillRequiresAPositiveLimit(t *testing.T) {
	_, s, _, _ := wakeFixture(t)
	if _, err := s.BackfillLegacyWakeupDefinitions(context.Background(), LegacyBackfillRequest{}); !errors.Is(err, ErrWakeupInput) {
		t.Fatalf("zero limit = %v, want ErrWakeupInput", err)
	}
}

// Production must keep zero persisted scoped definitions until the layer that
// opens definition writes activates the backfill: nothing outside tests may
// call it, in particular no startup or scheduler hook.
func TestLegacyBackfillIsNotWiredIntoProduction(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Base(path) == "wakeup_legacy_backfill.go" || strings.Contains(path, "generated") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range []string{"RunLegacyWakeupBackfill", "BackfillLegacyWakeupDefinitions", "InsertWakeupDefinitionIfAbsent"} {
			if strings.Contains(string(src), name) {
				t.Errorf("%s references %s; the backfill must stay unwired until definition writes are opened", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
