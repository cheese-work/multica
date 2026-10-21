package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1727 review (round 2): the pause must survive a concurrent settings
// save, a failed guard read and a failed audit write, and cron schedules must
// skip the slots a pause missed.

func workspacePaused(t *testing.T, ws string) bool {
	t.Helper()
	var paused bool
	dbfx.QueryRow(t, `SELECT COALESCE(settings->>'wakeups_paused'='true',false) FROM workspace WHERE id=$1`, ws).Scan(&paused)
	return paused
}

func TestStaleSettingsSaveCannotClearAConcurrentPause(t *testing.T) {
	ws := pauseWorkspaceFixture(t, "wakeup-pause-stale-save")
	ctx := context.Background()
	// Hold the workspace row, as a concurrent pause flip does between a save's
	// read of the settings and its write.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workspace WHERE id=$1 FOR UPDATE`, ws); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+ws, map[string]any{"settings": map[string]any{"github_pr_sidebar_enabled": false}}), "id", ws)
		rec := httptest.NewRecorder()
		testHandler.UpdateWorkspace(rec, req)
		done <- rec.Code
	}()
	time.Sleep(500 * time.Millisecond)
	if _, err := tx.Exec(ctx, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true}'::jsonb WHERE id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("unrelated save returned %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unrelated save never finished")
	}
	if !workspacePaused(t, ws) {
		t.Fatal("a save that read the settings before the pause flip cleared the pause")
	}
	var sidebarOff bool
	dbfx.QueryRow(t, `SELECT settings->>'github_pr_sidebar_enabled'='false' FROM workspace WHERE id=$1`, ws).Scan(&sidebarOff)
	if !sidebarOff {
		t.Fatal("the unrelated save was lost")
	}
}

var errInjectedRead = errors.New("injected settings read failure")

type settingsReadErrRow struct{ err error }

func (r settingsReadErrRow) Scan(...any) error { return r.err }

func failsSettingsRead(sql string) bool {
	return strings.Contains(sql, "FROM workspace\nWHERE id = $1")
}

type readFailingDB struct{ db.DBTX }

func (d readFailingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if failsSettingsRead(sql) {
		return settingsReadErrRow{errInjectedRead}
	}
	return d.DBTX.QueryRow(ctx, sql, args...)
}

type readFailingTx struct{ pgx.Tx }

func (t readFailingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if failsSettingsRead(sql) {
		return settingsReadErrRow{errInjectedRead}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

type readFailingStarter struct{}

func (readFailingStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := testPool.Begin(ctx)
	return readFailingTx{tx}, err
}

func TestSettingsWriteFailsClosedWhenTheStoredSettingsCannotBeRead(t *testing.T) {
	ws := pauseWorkspaceFixture(t, "wakeup-pause-read-error")
	saveWorkspaceSettings(t, ws, map[string]any{"wakeups_paused": true})
	queries, starter := testHandler.Queries, testHandler.TxStarter
	testHandler.Queries, testHandler.TxStarter = db.New(readFailingDB{testPool}), readFailingStarter{}
	t.Cleanup(func() { testHandler.Queries, testHandler.TxStarter = queries, starter })
	// A non-object body would replace the paused settings; so would an object that
	// omits the switch, if the guard could not see the stored pause.
	for _, body := range []any{[]any{}, map[string]any{"github_pr_sidebar_enabled": false}} {
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+ws, map[string]any{"settings": body}), "id", ws)
		rec := httptest.NewRecorder()
		testHandler.UpdateWorkspace(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("settings write %v succeeded although the stored settings could not be read: %s", body, rec.Body.String())
		}
	}
	testHandler.Queries, testHandler.TxStarter = queries, starter
	if !workspacePaused(t, ws) {
		t.Fatal("a write that could not read the stored settings cleared the pause")
	}
	if n := pauseActivities(t, ws, "wakeups_resumed"); n != 0 {
		t.Fatalf("resume records = %d, want 0", n)
	}
}

func failAuditRecords(t *testing.T, ws string) {
	t.Helper()
	name := "che1727_fail_audit_" + strings.ReplaceAll(ws[:8], "-", "")
	dbfx.Exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.workspace_id = '%s'::uuid AND NEW.action IN ('wakeups_paused','wakeups_resumed') THEN RAISE EXCEPTION 'injected audit failure'; END IF;
		RETURN NEW; END $$`, name, ws))
	dbfx.Exec(t, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON activity_log FOR EACH ROW EXECUTE FUNCTION %s()`, name, name))
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON activity_log`, name))
		_, _ = testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
}

func TestPauseFlipRollsBackWhenItsAuditRecordFails(t *testing.T) {
	ws := pauseWorkspaceFixture(t, "wakeup-pause-audit-rollback")
	failAuditRecords(t, ws)
	patch := func(paused bool) int {
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+ws, map[string]any{"settings": map[string]any{"wakeups_paused": paused}}), "id", ws)
		rec := httptest.NewRecorder()
		testHandler.UpdateWorkspace(rec, req)
		return rec.Code
	}
	if code := patch(true); code == http.StatusOK {
		t.Fatal("a pause whose audit record could not be written reported success")
	}
	if workspacePaused(t, ws) {
		t.Fatal("the pause stayed on although its audit record rolled back")
	}
	// Resume rolls back too, and so does what resume settled.
	dbfx.Exec(t, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true,"wakeups_paused_by":"u"}'::jsonb WHERE id=$1`, ws)
	if code := patch(false); code == http.StatusOK {
		t.Fatal("a resume whose audit record could not be written reported success")
	}
	if !workspacePaused(t, ws) {
		t.Fatal("the pause was lifted although the resume record rolled back")
	}
}

func TestResumeBeforeNextTickSkipsMissedCronSlots(t *testing.T) {
	e := newResumeEnv(t, "wakeup-resume-cron")
	w := e.create(t, service.WakeupInput{Kind: "cron", CronExpression: "0 * * * *", Timezone: "UTC", Instruction: "tick"})
	saveWorkspaceSettings(t, e.ws, map[string]any{"wakeups_paused": true})
	dbfx.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '3 hours' WHERE id=$1", w.ID)
	saveWorkspaceSettings(t, e.ws, map[string]any{"wakeups_paused": false})
	e.tick(t)
	if n := e.runs(t, w); n != 0 {
		t.Fatalf("resume fired %d cron slots missed during the pause", n)
	}
	var future bool
	dbfx.QueryRow(t, `SELECT next_fire_at>now() FROM issue_wakeup WHERE id=$1`, w.ID).Scan(&future)
	if !future {
		t.Fatal("the next cron slot must be in the future after resume")
	}
}

// The pause switch sends only itself. The server applies it to the settings it
// reads under the workspace lock, so a client's cached snapshot of the whole blob
// can never overwrite a concurrent unrelated change.
func TestPauseSwitchPatchKeepsUnrelatedSettings(t *testing.T) {
	ws := pauseWorkspaceFixture(t, "wakeup-pause-partial")
	dbfx.Exec(t, `UPDATE workspace SET settings='{"github_pr_sidebar_enabled":false,"co_authored_by_enabled":false}'::jsonb WHERE id=$1`, ws)
	patch := func(paused bool) map[string]any {
		t.Helper()
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+ws, map[string]any{"wakeups_paused": paused}), "id", ws)
		var out struct {
			Settings map[string]any `json:"settings"`
		}
		testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK).JSON(&out)
		return out.Settings
	}
	got := patch(true)
	if got["wakeups_paused"] != true || got["wakeups_paused_by"] != testUserID || got["wakeups_paused_at"] == nil {
		t.Fatalf("pause not applied and stamped: %v", got)
	}
	if got["github_pr_sidebar_enabled"] != false || got["co_authored_by_enabled"] != false {
		t.Fatalf("pause patch dropped unrelated settings: %v", got)
	}
	if n := pauseActivities(t, ws, "wakeups_paused"); n != 1 {
		t.Fatalf("pause activity records = %d, want 1", n)
	}
	resumed := patch(false)
	if _, paused := resumed["wakeups_paused"]; paused || resumed["github_pr_sidebar_enabled"] != false {
		t.Fatalf("resume patch result: %v", resumed)
	}
	if n := pauseActivities(t, ws, "wakeups_resumed"); n != 1 {
		t.Fatalf("resume activity records = %d, want 1", n)
	}
}

// Retrospective (written with the fix, not before it): the resume time the
// server stamps survives a client's full-settings save, even one built from a
// snapshot that never had the key.
func TestResumeStampSurvivesAFullSettingsSave(t *testing.T) {
	ws := pauseWorkspaceFixture(t, "wakeup-resume-stamp")
	patch := func(body map[string]any) map[string]any {
		t.Helper()
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+ws, body), "id", ws)
		var out struct {
			Settings map[string]any `json:"settings"`
		}
		testutil.Call(t, testHandler.UpdateWorkspace, req).Want(http.StatusOK).JSON(&out)
		return out.Settings
	}
	patch(map[string]any{"wakeups_paused": true})
	resumed := patch(map[string]any{"wakeups_paused": false})
	stamp, _ := resumed["wakeups_resumed_at"].(string)
	if stamp == "" {
		t.Fatalf("resume did not stamp the resume time: %v", resumed)
	}
	forged := patch(map[string]any{"settings": map[string]any{"github_pr_sidebar_enabled": false, "wakeups_resumed_at": "2001-01-01T00:00:00Z"}})
	if forged["wakeups_resumed_at"] != stamp {
		t.Fatalf("a full settings save changed the server-owned resume time: %v", forged)
	}
}
