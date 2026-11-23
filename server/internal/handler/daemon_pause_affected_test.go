package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type affectedList struct {
	Machines []AffectedMachineResponse `json:"machines"`
}

func affectedAs(t *testing.T, userID string) []AffectedMachineResponse {
	t.Helper()
	return testutil.Decode[affectedList](t, testHandler.ListAffectedMachines,
		newRequestAs(userID, http.MethodGet, "/api/runtimes/affected", nil), http.StatusOK).Machines
}

func affectedFor(machines []AffectedMachineResponse, daemonID string) *AffectedMachineResponse {
	for i := range machines {
		if machines[i].DaemonID == daemonID {
			return &machines[i]
		}
	}
	return nil
}

// seedProjectTask queues a task for runtimeID on an issue of projectID ("" for none).
func seedProjectTask(t *testing.T, runtimeID, name, projectID string) {
	t.Helper()
	agentID := dbfx.Agent(t, name+" agent", runtimeID)
	over := testutil.Cols{}
	if projectID != "" {
		over["project_id"] = projectID
	}
	issueID := dbfx.Issue(t, name+" issue", over)
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
}

func TestAffectedMachines_PausedMachineWithQueuedTaskReachesWorkspaceOwner(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-paused"
	seedPausableRuntime(t, context.Background(), daemonID, "Affected paused")
	pauseDaemonForTest(t, daemonID)

	got := affectedFor(affectedAs(t, testUserID), daemonID)
	if got == nil {
		t.Fatal("paused machine with a queued task is missing from the workspace owner's list")
	}
	if got.State != "paused" || got.Reason == nil || *got.Reason != "maintenance" || got.WaitingTasks != 1 {
		t.Fatalf("got %+v, want paused/maintenance with 1 waiting task", got)
	}
	if got.ExpiresAt == nil || !strings.HasPrefix(got.EventID, "paused:") {
		t.Fatalf("got expires_at=%v event_id=%q, want an expiry and a paused: event id", got.ExpiresAt, got.EventID)
	}
}

func TestAffectedMachines_OnlineMachineAndMachineWithoutQueuedTaskAreNotAffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	seedPausableRuntime(t, ctx, "affected-online", "Affected online")
	dbfx.Runtime(t, "Affected idle", testutil.Cols{"daemon_id": "affected-idle"})
	pauseDaemonForTest(t, "affected-idle")

	machines := affectedAs(t, testUserID)
	for _, id := range []string{"affected-online", "affected-idle"} {
		if affectedFor(machines, id) != nil {
			t.Fatalf("machine %s listed, want none: it is online or nothing waits for it", id)
		}
	}
}

func TestAffectedMachines_OfflineMachineWithQueuedTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-offline"
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Affected offline")
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeID)

	got := affectedFor(affectedAs(t, testUserID), daemonID)
	if got == nil || got.State != "offline" || got.Reason != nil || got.ExpiresAt != nil || got.WaitingTasks != 1 {
		t.Fatalf("got %+v, want offline, no reason, no expiry, 1 waiting task", got)
	}
	if !strings.HasPrefix(got.EventID, "offline:") {
		t.Fatalf("event id %q, want offline: prefix", got.EventID)
	}
}

func TestAffectedMachines_OneOnlineRuntimeKeepsTheMachineOn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-partial"
	runtimeA, _ := seedPausableRuntime(t, context.Background(), daemonID, "Affected partial A")
	dbfx.Runtime(t, "Affected partial B", testutil.Cols{"daemon_id": daemonID, "provider": "handler_test_runtime_b"})
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeA)

	if affectedFor(affectedAs(t, testUserID), daemonID) != nil {
		t.Fatal("machine with an online runtime listed as offline")
	}
}

func TestAffectedMachines_ExpiredPauseIsNotAffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-expired"
	seedPausableRuntime(t, context.Background(), daemonID, "Affected expired")
	pauseDaemonForTest(t, daemonID)
	dbfx.Exec(t, `UPDATE daemon_pause SET paused_at = now() - interval '3 hours' WHERE daemon_id = $1`, daemonID)

	if affectedFor(affectedAs(t, testUserID), daemonID) != nil {
		t.Fatal("machine whose pause expired listed as affected")
	}
}

func TestAffectedMachines_ProjectLeadSeesOnlyTheirProjects(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leadID := dbfx.User(t, "Affected lead", "affected-lead@multica.test")
	dbfx.Member(t, testWorkspaceID, leadID, "member")
	otherID := dbfx.User(t, "Affected other", "affected-other@multica.test")
	dbfx.Member(t, testWorkspaceID, otherID, "member")
	led := dbfx.Project(t, "Affected led", testutil.Cols{"lead_type": "member", "lead_id": leadID})
	notLed := dbfx.Project(t, "Affected not led", testutil.Cols{"lead_type": "member", "lead_id": otherID})

	for _, c := range []struct{ daemon, project string }{{"affected-lead-led", led}, {"affected-lead-other", notLed}, {"affected-lead-none", ""}} {
		runtimeID := dbfx.Runtime(t, "Affected "+c.daemon, testutil.Cols{"daemon_id": c.daemon})
		seedProjectTask(t, runtimeID, c.daemon, c.project)
		pauseDaemonForTest(t, c.daemon)
	}

	lead := affectedAs(t, leadID)
	if affectedFor(lead, "affected-lead-led") == nil {
		t.Fatal("project lead misses the machine that holds their project's task")
	}
	for _, id := range []string{"affected-lead-other", "affected-lead-none"} {
		if affectedFor(lead, id) != nil {
			t.Fatalf("project lead sees %s, which holds no task of their project", id)
		}
	}
	w := httptest.NewRecorder()
	testHandler.ListAffectedMachines(w, newRequestAs(dbfx.User(t, "Affected plain", "affected-plain@multica.test"), http.MethodGet, "/api/runtimes/affected", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-member = %d, want 404", w.Code)
	}
	owner := affectedAs(t, testUserID)
	for _, id := range []string{"affected-lead-led", "affected-lead-other", "affected-lead-none"} {
		if affectedFor(owner, id) == nil {
			t.Fatalf("workspace owner misses %s, a paused machine holding a queued task", id)
		}
	}
}

// The machine identity (owner_id + daemon_id) can span workspaces. The viewer
// of workspace A must get A's device label, never a runtime name or device
// string that belongs to workspace B, which they cannot read.
func TestAffectedMachines_DeviceLabelDoesNotLeakAcrossWorkspaces(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-leak"
	seedPausableRuntime(t, context.Background(), daemonID, "Leak A runtime")
	pauseDaemonForTest(t, daemonID)
	otherWS := dbfx.Workspace(t, "Leak other", "affected-leak-other")
	dbfx.Runtime(t, "Leak B runtime", testutil.Cols{
		"workspace_id": otherWS, "daemon_id": daemonID, "device_info": "AAA-secret-device-of-B",
	})

	got := affectedFor(affectedAs(t, testUserID), daemonID)
	if got == nil {
		t.Fatal("paused machine missing from the list")
	}
	if strings.Contains(got.DeviceName, "secret") || strings.Contains(got.DeviceName, "Leak B") {
		t.Fatalf("device name %q leaks workspace B metadata", got.DeviceName)
	}
	if got.DeviceName != "Leak A runtime" {
		t.Fatalf("device name = %q, want workspace A's runtime name", got.DeviceName)
	}
}

// ---- Server-stored banner dismissals (CHE-9190, step 1) ----

type bannerRow struct {
	DaemonID  string `json:"daemon_id"`
	State     string `json:"state"`
	EventID   string `json:"event_id"`
	Dismissed *bool  `json:"dismissed"`
}

type bannerList struct {
	BannerDismissal *string     `json:"banner_dismissal"`
	Machines        []bannerRow `json:"machines"`
}

// setBannerCapability flips the switch for one test and restores it after.
func setBannerCapability(t *testing.T, value string) {
	t.Helper()
	prev := bannerDismissalCapability
	bannerDismissalCapability = value
	t.Cleanup(func() { bannerDismissalCapability = prev })
}

func bannerGet(t *testing.T, userID string) bannerList {
	t.Helper()
	return testutil.Decode[bannerList](t, testHandler.ListAffectedMachines,
		newRequestAs(userID, http.MethodGet, "/api/runtimes/affected", nil), http.StatusOK)
}

func bannerRowOf(t *testing.T, userID, daemonID string) bannerRow {
	t.Helper()
	for _, m := range bannerGet(t, userID).Machines {
		if m.DaemonID == daemonID {
			return m
		}
	}
	t.Fatalf("machine %s is missing from the affected list of %s", daemonID, userID)
	return bannerRow{}
}

func isDismissed(t *testing.T, userID, daemonID string) bool {
	t.Helper()
	row := bannerRowOf(t, userID, daemonID)
	if row.Dismissed == nil {
		t.Fatalf("machine %s has no dismissed field: %+v", daemonID, row)
	}
	return *row.Dismissed
}

// bannerPost calls dismiss (key "dismissed") or reopen (key "reopened").
func bannerPost(t *testing.T, h http.HandlerFunc, userID, path string, ids ...string) map[string][]string {
	t.Helper()
	req := newRequestAs(userID, http.MethodPost, "/api/runtimes/affected/"+path, map[string]any{"event_ids": ids})
	return testutil.Decode[map[string][]string](t, h, req, http.StatusOK)
}

func dismissIDs(t *testing.T, userID string, ids ...string) map[string][]string {
	t.Helper()
	return bannerPost(t, testHandler.DismissAffectedMachines, userID, "dismiss", ids...)
}

func reopenIDs(t *testing.T, userID string, ids ...string) map[string][]string {
	t.Helper()
	return bannerPost(t, testHandler.ReopenAffectedMachines, userID, "reopen", ids...)
}

func bannerRowCount(t *testing.T, daemonID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM machine_banner_dismissal WHERE daemon_id = $1`, daemonID)
}

func cleanupBannerRows(t *testing.T, daemonPrefix string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM machine_banner_dismissal WHERE daemon_id LIKE $1`, daemonPrefix+"%")
	})
}

// setRuntimeOffline marks a runtime offline with last_seen_at that many seconds ago.
func setRuntimeOffline(t *testing.T, runtimeID string, secondsAgo int) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline', last_seen_at = date_trunc('second', now()) - make_interval(secs => $2) WHERE id = $1`, runtimeID, secondsAgo)
}

func setRuntimeOnline(t *testing.T, runtimeID string, secondsAgo int) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'online', last_seen_at = date_trunc('second', now()) - make_interval(secs => $2) WHERE id = $1`, runtimeID, secondsAgo)
}

// seedOfflineMachine seeds an offline machine holding one queued task for the workspace owner.
func seedOfflineMachine(t *testing.T, daemonID string, secondsAgo int) string {
	t.Helper()
	cleanupBannerRows(t, daemonID)
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, daemonID)
	setRuntimeOffline(t, runtimeID, secondsAgo)
	return runtimeID
}

func TestAffectedMachines_T0_PinnedDatabaseHasTheTable(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica_test?sslmode=disable"
	}
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	var name string
	var table *string
	dbfx.QueryRow(t, `SELECT current_database(), to_regclass('machine_banner_dismissal')::text`).Scan(&name, &table)
	if want := strings.TrimPrefix(parsed.Path, "/"); name != want {
		t.Fatalf("current_database() = %q, DATABASE_URL names %q", name, want)
	}
	if table == nil {
		t.Fatalf("table machine_banner_dismissal does not exist in %s", name)
	}
}

// T1: server state has no session, so a new GET after a dismiss is the "new
// session or other device" case. Red on main: there is no endpoint.
func TestAffectedMachines_DismissSurvivesNewSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t1"
	seedOfflineMachine(t, daemonID, 3600)

	row := bannerRowOf(t, testUserID, daemonID)
	if row.Dismissed == nil || *row.Dismissed {
		t.Fatalf("before dismiss: %+v, want dismissed=false", row)
	}
	got := dismissIDs(t, testUserID, row.EventID)
	if len(got["dismissed"]) != 1 || len(got["ignored"]) != 0 {
		t.Fatalf("dismiss response = %v, want the id dismissed and none ignored", got)
	}
	if list := bannerGet(t, testUserID); list.BannerDismissal == nil || *list.BannerDismissal != "epoch-v1" {
		t.Fatalf("banner_dismissal = %v, want epoch-v1", list.BannerDismissal)
	}
	if !isDismissed(t, testUserID, daemonID) {
		t.Fatal("a new GET as the same user reports dismissed=false after the dismiss")
	}
}

// T2: the dismissal belongs to the member who closed the banner.
func TestAffectedMachines_DismissIsPerMember(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t2"
	cleanupBannerRows(t, daemonID)
	leadID := dbfx.User(t, "Dismiss lead", "dismiss-lead@multica.test")
	dbfx.Member(t, testWorkspaceID, leadID, "member")
	led := dbfx.Project(t, "Dismiss led", testutil.Cols{"lead_type": "member", "lead_id": leadID})
	runtimeID := dbfx.Runtime(t, "Dismiss t2", testutil.Cols{"daemon_id": daemonID})
	seedProjectTask(t, runtimeID, daemonID, led)
	setRuntimeOffline(t, runtimeID, 3600)

	dismissIDs(t, testUserID, bannerRowOf(t, testUserID, daemonID).EventID)
	if !isDismissed(t, testUserID, daemonID) {
		t.Fatal("the owner who dismissed sees dismissed=false")
	}
	if isDismissed(t, leadID, daemonID) {
		t.Fatal("a project lead who did not dismiss sees the owner's dismissal")
	}
}

// T3: a new offline period gets a new event id, so the old dismissal no longer applies.
func TestAffectedMachines_NewOfflinePeriodIsNotDismissed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t3"
	runtimeID := seedOfflineMachine(t, daemonID, 3600)
	first := bannerRowOf(t, testUserID, daemonID).EventID
	dismissIDs(t, testUserID, first)

	setRuntimeOnline(t, runtimeID, 1800)
	setRuntimeOffline(t, runtimeID, 600)
	row := bannerRowOf(t, testUserID, daemonID)
	if row.EventID == first {
		t.Fatalf("event id %q did not change for a new offline period", row.EventID)
	}
	if row.Dismissed == nil || *row.Dismissed {
		t.Fatalf("new period: %+v, want dismissed=false", row)
	}
}

// T4 (r1 P0 case): after a runtime delete the MAX falls to another runtime's
// value. The id differs from the stored one, so the banner shows.
func TestAffectedMachines_DeleteOfMaxRuntimeShowsBanner(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t4"
	seedOfflineMachine(t, daemonID, 10800)
	runtimeB := dbfx.Runtime(t, "Dismiss t4 B", testutil.Cols{"daemon_id": daemonID, "provider": "handler_test_runtime_b"})
	setRuntimeOffline(t, runtimeB, 7200)
	stored := bannerRowOf(t, testUserID, daemonID).EventID
	dismissIDs(t, testUserID, stored)

	setRuntimeOnline(t, runtimeB, 3600)
	setRuntimeOffline(t, runtimeB, 1800)
	dbfx.Exec(t, `DELETE FROM agent_runtime WHERE id = $1`, runtimeB)

	row := bannerRowOf(t, testUserID, daemonID)
	if row.EventID == stored {
		t.Fatalf("event id %q equals the stored one after the MAX runtime was deleted", row.EventID)
	}
	if row.Dismissed == nil || *row.Dismissed {
		t.Fatalf("after deleting the MAX runtime: %+v, want dismissed=false", row)
	}
}

// T5: deleting an idle runtime that does not hold the MAX keeps the id and the dismissal.
func TestAffectedMachines_DeleteOfIdleRuntimeKeepsDismissal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t5"
	seedOfflineMachine(t, daemonID, 1800)
	runtimeB := dbfx.Runtime(t, "Dismiss t5 B", testutil.Cols{"daemon_id": daemonID, "provider": "handler_test_runtime_b"})
	setRuntimeOffline(t, runtimeB, 7200)
	stored := bannerRowOf(t, testUserID, daemonID).EventID
	dismissIDs(t, testUserID, stored)

	dbfx.Exec(t, `DELETE FROM agent_runtime WHERE id = $1`, runtimeB)

	row := bannerRowOf(t, testUserID, daemonID)
	if row.EventID != stored {
		t.Fatalf("event id changed from %q to %q after deleting an idle runtime", stored, row.EventID)
	}
	if row.Dismissed == nil || !*row.Dismissed {
		t.Fatalf("after deleting an idle runtime: %+v, want dismissed=true", row)
	}
}

// T6: stale, unknown, paused and foreign ids go to ignored and store nothing.
func TestAffectedMachines_UnknownStalePausedAndForeignIDsAreIgnored(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const offlineDaemon, pausedDaemon, otherDaemon = "dismiss-t6", "dismiss-t6-paused", "dismiss-t6-other"
	cleanupBannerRows(t, "dismiss-t6")
	runtimeID := seedOfflineMachine(t, offlineDaemon, 3600)
	stale := bannerRowOf(t, testUserID, offlineDaemon).EventID
	setRuntimeOnline(t, runtimeID, 1800)
	setRuntimeOffline(t, runtimeID, 600)

	seedPausableRuntime(t, context.Background(), pausedDaemon, "Dismiss t6 paused")
	pauseDaemonForTest(t, pausedDaemon)
	paused := bannerRowOf(t, testUserID, pausedDaemon).EventID

	seedOfflineMachine(t, otherDaemon, 3600)
	foreign := bannerRowOf(t, testUserID, otherDaemon).EventID
	outsiderID := dbfx.User(t, "Dismiss outsider", "dismiss-outsider@multica.test")
	dbfx.Member(t, testWorkspaceID, outsiderID, "member")

	got := dismissIDs(t, testUserID, stale, paused, "offline:nobody/unknown:1")
	if len(got["dismissed"]) != 0 || len(got["ignored"]) != 3 {
		t.Fatalf("owner dismiss = %v, want 3 ignored and none dismissed", got)
	}
	got = dismissIDs(t, outsiderID, foreign)
	if len(got["dismissed"]) != 0 || len(got["ignored"]) != 1 {
		t.Fatalf("foreign dismiss = %v, want 1 ignored and none dismissed", got)
	}
	for _, daemon := range []string{offlineDaemon, pausedDaemon, otherDaemon} {
		if n := bannerRowCount(t, daemon); n != 0 {
			t.Fatalf("%s has %d stored rows, want 0", daemon, n)
		}
	}
}

// T7: reopen clears the current dismissal; an older id leaves it in place.
func TestAffectedMachines_ReopenClearsOnlyTheNamedPeriod(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t7"
	runtimeID := seedOfflineMachine(t, daemonID, 3600)
	old := bannerRowOf(t, testUserID, daemonID).EventID
	setRuntimeOnline(t, runtimeID, 1800)
	setRuntimeOffline(t, runtimeID, 600)
	current := bannerRowOf(t, testUserID, daemonID).EventID
	dismissIDs(t, testUserID, current)

	got := reopenIDs(t, testUserID, old)
	if len(got["reopened"]) != 0 || len(got["ignored"]) != 1 {
		t.Fatalf("reopen of an older id = %v, want it ignored", got)
	}
	if !isDismissed(t, testUserID, daemonID) {
		t.Fatal("reopen with an older id cleared the current dismissal")
	}
	got = reopenIDs(t, testUserID, current)
	if len(got["reopened"]) != 1 || len(got["ignored"]) != 0 {
		t.Fatalf("reopen of the current id = %v, want it reopened", got)
	}
	if isDismissed(t, testUserID, daemonID) {
		t.Fatal("reopen of the current id left dismissed=true")
	}
}

func seedBannerRow(t *testing.T, workspaceID, userID, daemonID string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO machine_banner_dismissal (workspace_id, user_id, machine_owner_id, daemon_id, event_id) VALUES ($1, $2, gen_random_uuid(), $3, 'offline:x:1')`,
		workspaceID, userID, daemonID)
}

// T8: workspace delete and member revoke remove that scope's rows, and only those.
func TestAffectedMachines_WorkspaceDeleteAndMemberRevokeRemoveRows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cleanupBannerRows(t, "dismiss-t8")

	t.Run("workspace delete", func(t *testing.T) {
		const slug = "dismiss-t8-ws"
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE slug = $1`, slug)
		wsID := dbfx.Insert(t, "workspace", testutil.Cols{"name": "Dismiss t8", "slug": slug, "description": "t8"})
		dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, wsID, testUserID)
		seedBannerRow(t, wsID, testUserID, "dismiss-t8-ws-gone")
		seedBannerRow(t, testWorkspaceID, testUserID, "dismiss-t8-ws-kept")

		req := withURLParam(newRequest("DELETE", "/api/workspaces/"+wsID, nil), "id", wsID)
		testutil.Call(t, testHandler.DeleteWorkspace, req).Want(http.StatusNoContent)

		if n := bannerRowCount(t, "dismiss-t8-ws-gone"); n != 0 {
			t.Fatalf("deleted workspace left %d dismissal rows", n)
		}
		if n := bannerRowCount(t, "dismiss-t8-ws-kept"); n != 1 {
			t.Fatalf("other workspace has %d dismissal rows, want 1", n)
		}
	})

	t.Run("member revoke", func(t *testing.T) {
		fx := setupRevocationFixture(t, "dismiss-t8-revoke", "dismiss-t8-revoke-daemon")
		seedBannerRow(t, fx.WorkspaceID, fx.TargetUserID, "dismiss-t8-rv-target")
		seedBannerRow(t, fx.WorkspaceID, testUserID, "dismiss-t8-rv-other-member")
		seedBannerRow(t, testWorkspaceID, fx.TargetUserID, "dismiss-t8-rv-other-ws")

		req := newRequest("DELETE", "/api/workspaces/"+fx.WorkspaceID+"/members/"+fx.MemberID, nil)
		req.Header.Set("X-Workspace-ID", fx.WorkspaceID)
		req = withURLParams(req, "id", fx.WorkspaceID, "memberId", fx.MemberID)
		testutil.Call(t, testHandler.DeleteMember, req).Want(http.StatusNoContent)

		if n := bannerRowCount(t, "dismiss-t8-rv-target"); n != 0 {
			t.Fatalf("revoked member left %d rows in the workspace", n)
		}
		for _, daemon := range []string{"dismiss-t8-rv-other-member", "dismiss-t8-rv-other-ws"} {
			if n := bannerRowCount(t, daemon); n != 1 {
				t.Fatalf("%s has %d rows, want 1", daemon, n)
			}
		}
	})
}

// T9: a paused row always reports dismissed=false, even after a dismiss call with its id.
func TestAffectedMachines_PausedRowIsNeverDismissed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "epoch-v1")
	const daemonID = "dismiss-t9"
	cleanupBannerRows(t, daemonID)
	seedPausableRuntime(t, context.Background(), daemonID, "Dismiss t9")
	pauseDaemonForTest(t, daemonID)
	row := bannerRowOf(t, testUserID, daemonID)
	if row.State != "paused" || row.Dismissed == nil || *row.Dismissed {
		t.Fatalf("paused row = %+v, want state paused and dismissed=false", row)
	}
	dismissIDs(t, testUserID, row.EventID)
	if isDismissed(t, testUserID, daemonID) {
		t.Fatal("a paused row reports dismissed=true after a dismiss call")
	}
}

// T11: with the switch unset the feature is off.
func TestAffectedMachines_SwitchUnsetMeansNoDismissalFieldsAndPost409(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setBannerCapability(t, "")
	const daemonID = "dismiss-t11"
	seedOfflineMachine(t, daemonID, 3600)

	raw := testutil.Decode[map[string]any](t, testHandler.ListAffectedMachines,
		newRequestAs(testUserID, http.MethodGet, "/api/runtimes/affected", nil), http.StatusOK)
	if _, ok := raw["banner_dismissal"]; ok {
		t.Fatalf("GET has banner_dismissal while the switch is unset: %v", raw)
	}
	for _, m := range raw["machines"].([]any) {
		if _, ok := m.(map[string]any)["dismissed"]; ok {
			t.Fatalf("a machine has dismissed while the switch is unset: %v", m)
		}
	}
	for path, h := range map[string]http.HandlerFunc{"dismiss": testHandler.DismissAffectedMachines, "reopen": testHandler.ReopenAffectedMachines} {
		req := newRequestAs(testUserID, http.MethodPost, "/api/runtimes/affected/"+path, map[string]any{"event_ids": []string{"offline:x:1"}})
		got := testutil.Decode[map[string]string](t, h, req, http.StatusConflict)
		if got["error"] != "banner_dismissal_unavailable" {
			t.Fatalf("%s error = %q, want banner_dismissal_unavailable", path, got["error"])
		}
	}
	if n := bannerRowCount(t, daemonID); n != 0 {
		t.Fatalf("%d rows stored while the switch is unset", n)
	}
}
