package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// loadChildDone resolves child_done for the issue the way capture and dispatch do.
func loadChildDone(t *testing.T, s *IssueWakeupService, issue pgtype.UUID) *builtinWakeup {
	t.Helper()
	ctx := context.Background()
	row, err := s.Tasks.Queries.GetIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	var legacy *db.IssueWakeup
	if w, err := s.Tasks.Queries.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRuleChildDone)}); err == nil {
		legacy = &w
	}
	b, err := loadBuiltinWakeup(ctx, s.Tasks.Queries, row, SystemRuleChildDone, legacy)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// useProductionActivation points capture at the fixture's own activation row.
func useProductionActivation(t *testing.T, s *IssueWakeupService) {
	t.Helper()
	previous := wakeupActivationKey
	wakeupActivationKey = s.ActivationKey
	t.Cleanup(func() { wakeupActivationKey = previous })
}

// While the backfill is paging, the definitions it already wrote must not change
// what capture and dispatch run: the legacy path stays in charge until the
// activation completed.
func TestActivationKeepsCaptureOnTheLegacyPathUntilItCompletes(t *testing.T) {
	f, s, issue := activationFixture(t)
	useProductionActivation(t, s)
	ws := parseTestUUID(t, f.WorkspaceID)
	off := false
	customizeChildDone(t, s, issue, &off, nil)
	second := parseTestUUID(t, f.Issue(t, "second parent"))
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", second)
	text := "second text"
	customizeChildDone(t, s, second, nil, &text)

	part, err := s.ActivateWakeupDefinitions(context.Background(), ActivationRequest{Page: 1, MaxPages: 1, WorkspaceID: ws})
	if err != nil || part.Complete || part.Inserted != 1 {
		t.Fatalf("first page = %+v %v, want one definition and an open backfill", part, err)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_definition WHERE workspace_id=$1`, f.WorkspaceID); n != 1 {
		t.Fatalf("definitions after one page = %d, want 1", n)
	}
	for _, id := range []pgtype.UUID{issue, second} {
		if b := loadChildDone(t, s, id); b != nil {
			t.Fatalf("a stored definition reached capture before the activation completed: %+v", b.Eff)
		}
	}

	res, err := s.ActivateWakeupDefinitions(context.Background(), ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: ws})
	if err != nil || !res.Complete {
		t.Fatalf("completion = %+v %v", res, err)
	}
	for _, id := range []pgtype.UUID{issue, second} {
		if b := loadChildDone(t, s, id); b == nil {
			t.Fatalf("no definition resolved after the activation completed (issue %s)", uuidString(id))
		}
	}
}

// An older client keeps editing the legacy row. The backfilled override is a
// snapshot of it, so an edit after its page committed, or after the activation,
// must still be what runs, until a member writes the definition.
func TestOldClientEditsWinOverTheBackfilledSnapshot(t *testing.T) {
	f, s, issue := activationFixture(t)
	useProductionActivation(t, s)
	ws := parseTestUUID(t, f.WorkspaceID)
	ctx := context.Background()
	first := "first text"
	customizeChildDone(t, s, issue, nil, &first)

	// Edit while the activation is verifying: the page committed with the old text.
	if _, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 50, MaxPages: 1, WorkspaceID: ws}); err != nil {
		t.Fatal(err)
	}
	during := "edited during"
	customizeChildDone(t, s, issue, nil, &during)
	if res, err := s.ActivateWakeupDefinitions(ctx, ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: ws}); err != nil || !res.Complete {
		t.Fatalf("activation = %+v %v", res, err)
	}
	if got := loadChildDone(t, s, issue).Eff.Config.Instruction.Value; got != during {
		t.Fatalf("instruction after an edit during activation = %q, want %q", got, during)
	}

	after := "edited after"
	customizeChildDone(t, s, issue, nil, &after)
	if got := loadChildDone(t, s, issue).Eff.Config.Instruction.Value; got != after {
		t.Fatalf("instruction after an edit after activation = %q, want %q", got, after)
	}

	// A member's own write is not a snapshot: it wins over the legacy row.
	f.Exec(t, `UPDATE issue_wakeup_definition SET updated_by=$2 WHERE workspace_id=$1 AND scope_kind='issue'`, f.WorkspaceID, f.UserID)
	later := "legacy later"
	customizeChildDone(t, s, issue, nil, &later)
	if got := loadChildDone(t, s, issue).Eff.Config.Instruction.Value; got != first {
		t.Fatalf("a member-written definition lost to the legacy row: %q, want %q", got, first)
	}
}
