package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Machine history, audit and planned times (CHE-5709, plan D14).

func serverDeployCommits(t *testing.T) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT "commit" FROM server_deploy ORDER BY started_at, id`)
	if err != nil {
		t.Fatalf("read server_deploy: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func recordDeploy(t *testing.T, commit string) bool {
	t.Helper()
	wrote, err := testHandler.RecordServerDeploy(context.Background(), commit, "v1")
	if err != nil {
		t.Fatalf("RecordServerDeploy(%q): %v", commit, err)
	}
	return wrote
}

// Acceptance 1: a new commit writes one row; a restart on the same commit writes none.
func TestServerDeploy_OneRowPerNewCommit(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// The table is global, so this test owns it for its duration.
	testPool.Exec(context.Background(), `DELETE FROM server_deploy`)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM server_deploy`) })

	if !recordDeploy(t, "aaaaaaa") {
		t.Fatal("first start with a new commit wrote no row")
	}
	if recordDeploy(t, "aaaaaaa") {
		t.Fatal("restart on the same commit wrote a row")
	}
	if !recordDeploy(t, "bbbbbbb") {
		t.Fatal("start with a changed commit wrote no row")
	}
	// A rollback to an older commit is a deploy too: only the newest row counts.
	if !recordDeploy(t, "aaaaaaa") {
		t.Fatal("rollback to an older commit wrote no row")
	}
	for _, c := range []string{"", "unknown"} {
		if recordDeploy(t, c) {
			t.Fatalf("commit %q (a dev build) wrote a row", c)
		}
	}
	if got := fmt.Sprint(serverDeployCommits(t)); got != "[aaaaaaa bbbbbbb aaaaaaa]" {
		t.Fatalf("server_deploy commits = %s, want [aaaaaaa bbbbbbb aaaaaaa]", got)
	}
}

type machineCycleJSON struct {
	ID            string     `json:"id"`
	State         string     `json:"state"`
	Outcome       *string    `json:"outcome"`
	OutcomeDetail *string    `json:"outcome_detail"`
	FromCommit    *string    `json:"from_commit"`
	TargetCommit  *string    `json:"target_commit"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

type machineHistoryJSON struct {
	Cycles     []machineCycleJSON `json:"cycles"`
	NextCursor *string            `json:"next_cursor"`
}

func getMachine(t *testing.T, fn http.HandlerFunc, actorID, runtimeID, path, query string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequestAs(actorID, http.MethodGet, "/api/runtimes/"+runtimeID+"/machine/"+path+query, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	fn(w, withURLParam(req, "runtimeId", runtimeID))
	return w
}

func seedCycle(t *testing.T, ownerID, daemonID string, startedAgo time.Duration, outcome string, from, target string) string {
	t.Helper()
	id := uuid.NewString()
	started := time.Now().Add(-startedAgo).Truncate(time.Millisecond)
	var finished, out, source any
	state := "draining"
	if outcome != "" {
		state, out, source, finished = outcome, outcome, "server", started.Add(5*time.Minute)
	}
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO daemon_update_cycle (id, machine_owner_id, daemon_id, fire_at, started_at, deadline_at, state, from_commit, target_commit, outcome, outcome_detail, outcome_source, finished_at)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		id, ownerID, daemonID, started, started.Add(30*time.Minute), state, from, target, out, "detail "+outcome, source, finished); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}
	return id
}

// Acceptance 2: cycles newest first with each outcome, paged; unknown outcome strings pass through.
func TestMachineHistory_NewestFirstPaged(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "machine-history-a"
	runtimeID := dbfx.Runtime(t, "History A", testutil.Cols{"daemon_id": daemonID})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_update_cycle WHERE daemon_id IN ($1, $2)`, daemonID, "machine-history-other")
	})

	old := seedCycle(t, testUserID, daemonID, 72*time.Hour, "no_window", "c1", "c2")
	mid := seedCycle(t, testUserID, daemonID, 48*time.Hour, "updated", "c2", "c3")
	open := seedCycle(t, testUserID, daemonID, time.Hour, "", "c3", "c4")
	seedCycle(t, testUserID, "machine-history-other", 2*time.Hour, "updated", "x", "y") // another machine

	w := getMachine(t, testHandler.GetMachineHistory, testUserID, runtimeID, "history", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("history = %d: %s", w.Code, w.Body.String())
	}
	var page machineHistoryJSON
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range page.Cycles {
		ids = append(ids, c.ID)
	}
	if fmt.Sprint(ids) != fmt.Sprint([]string{open, mid, old}) {
		t.Fatalf("cycles = %v, want newest first %v (the other machine's cycle excluded)", ids, []string{open, mid, old})
	}
	if c := page.Cycles[0]; c.Outcome != nil || c.State != "draining" || c.FinishedAt != nil {
		t.Fatalf("open cycle = %+v, want no outcome, state draining, no end", c)
	}
	if c := page.Cycles[1]; c.Outcome == nil || *c.Outcome != "updated" || c.OutcomeDetail == nil || *c.OutcomeDetail != "detail updated" ||
		c.FromCommit == nil || *c.FromCommit != "c2" || c.TargetCommit == nil || *c.TargetCommit != "c3" || c.FinishedAt == nil {
		t.Fatalf("finished cycle = %+v, want outcome, detail, commits and end", c)
	}

	// Paging: limit 2, then the cursor gives the rest.
	var first machineHistoryJSON
	w = getMachine(t, testHandler.GetMachineHistory, testUserID, runtimeID, "history", "?limit=2", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil || len(first.Cycles) != 2 || first.NextCursor == nil {
		t.Fatalf("page 1 = %s (err %v), want 2 cycles and a cursor", w.Body.String(), err)
	}
	var second machineHistoryJSON
	w = getMachine(t, testHandler.GetMachineHistory, testUserID, runtimeID, "history", "?limit=2&cursor="+*first.NextCursor, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil || len(second.Cycles) != 1 || second.Cycles[0].ID != old || second.NextCursor != nil {
		t.Fatalf("page 2 = %s (err %v), want only the oldest cycle and no cursor", w.Body.String(), err)
	}
	if w = getMachine(t, testHandler.GetMachineHistory, testUserID, runtimeID, "history", "?cursor=bad", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400", w.Code)
	}
}

// Acceptance 3: only this machine's events; a member without access gets 404.
func TestMachineAudit_OnlyThisMachineAndAccess(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	f := newAuditReadFixture(t)
	mk := func(name, daemonID string) string {
		return dbfx.Runtime(t, name, testutil.Cols{"workspace_id": f.wsA, "owner_id": f.adminA, "daemon_id": daemonID, "provider": name})
	}
	rtA, rtA2 := mk("audit-a", "machine-audit-1"), mk("audit-a2", "machine-audit-1")
	rtOther := mk("audit-other", "machine-audit-2")
	now := time.Now()
	want := map[string]bool{}
	for i, s := range []struct{ rt, kind string }{
		{rtA, "runtime.pause"}, {rtA2, "runtime.resume"}, {rtA, "runtime.update"}, {rtA2, "runtime.update_schedule.set"},
		{rtOther, "runtime.pause"},  // another machine
		{rtA, "runtime.rename"},     // not a machine-tab kind
		{f.wsA, "workspace.rename"}, // not a runtime event
	} {
		id := seedAudit(t, seedRow{ws: f.wsA, kind: s.kind, targetType: "runtime", targetID: s.rt, at: now.Add(-time.Duration(i) * time.Minute)})
		want[id] = i < 4
	}
	// Another workspace's event on a runtime id of this machine must not leak.
	seedAudit(t, seedRow{ws: f.wsB, kind: "runtime.pause", targetType: "runtime", targetID: rtA, at: now})

	w := getMachine(t, testHandler.GetMachineAudit, f.adminA, rtA, "audit", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("audit = %d: %s", w.Code, w.Body.String())
	}
	var page auditReadPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ev := range page.Events {
		got[ev.ID] = true
	}
	for id, shown := range want {
		if got[id] != shown {
			t.Fatalf("event %s shown = %v, want %v (events: %+v)", id, got[id], shown, page.Events)
		}
	}
	if len(page.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(page.Events))
	}
	// Paged: limit 3 gives a cursor, the cursor gives the last one.
	var p1, p2 auditReadPage
	json.Unmarshal(getMachine(t, testHandler.GetMachineAudit, f.adminA, rtA, "audit", "?limit=3", nil).Body.Bytes(), &p1)
	if len(p1.Events) != 3 || p1.NextCursor == nil {
		t.Fatalf("audit page 1 = %+v, want 3 events and a cursor", p1)
	}
	json.Unmarshal(getMachine(t, testHandler.GetMachineAudit, f.adminA, rtA, "audit", "?limit=3&cursor="+*p1.NextCursor, nil).Body.Bytes(), &p2)
	if len(p2.Events) != 1 || p2.NextCursor != nil {
		t.Fatalf("audit page 2 = %+v, want 1 event", p2)
	}

	// No access: a plain member, a member of another workspace, and a stranger get 404.
	for name, actor := range map[string]string{"plain member": f.memberA, "other workspace admin": f.adminB, "non-member": f.nobody} {
		if w := getMachine(t, testHandler.GetMachineAudit, actor, rtA, "audit", "", nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s audit = %d, want 404: %s", name, w.Code, w.Body.String())
		}
	}
	// An agent token is refused, as on the workspace audit reader.
	if w := getMachine(t, testHandler.GetMachineAudit, f.adminA, rtA, "audit", "", map[string]string{"X-Actor-Source": "task_token"}); w.Code != http.StatusForbidden {
		t.Fatalf("task token audit = %d, want 403", w.Code)
	}
}

// History and planned follow runtime visibility: no access is 404, never 403.
func TestMachineHistoryAndPlanned_NoAccessIs404(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "machine-history-authz"
	runtimeID, _, plainMemberID := runtimeVisibilityFixture(t) // private runtime of another owner
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id = $2 WHERE id = $1`, runtimeID, daemonID)
	for name, fn := range map[string]http.HandlerFunc{"history": testHandler.GetMachineHistory, "planned": testHandler.GetMachinePlanned} {
		if w := getMachine(t, fn, plainMemberID, runtimeID, name, "", nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s for a member without access = %d, want 404: %s", name, w.Code, w.Body.String())
		}
		if w := getMachine(t, fn, testUserID, runtimeID, name, "", nil); w.Code != http.StatusOK {
			t.Fatalf("%s for the workspace owner = %d, want 200: %s", name, w.Code, w.Body.String())
		}
	}
}

// Planned: next 3 cron times of an enabled schedule and the server deploys of the last 24 h.
func TestMachinePlanned_NextTimesAndRecentDeploys(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "machine-planned"
	cleanupUpdateSchedule(t, daemonID)
	runtimeID := dbfx.Runtime(t, "Planned", testutil.Cols{"daemon_id": daemonID})
	testPool.Exec(context.Background(), `DELETE FROM server_deploy`)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM server_deploy`) })

	type planned struct {
		Timezone     string   `json:"timezone"`
		NextRuns     []string `json:"next_runs"`
		ServerDeploy []struct {
			Commit    string    `json:"commit"`
			Version   string    `json:"version"`
			StartedAt time.Time `json:"started_at"`
		} `json:"server_deploys"`
	}
	read := func() planned {
		w := getMachine(t, testHandler.GetMachinePlanned, testUserID, runtimeID, "planned", "", nil)
		var p planned
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("planned = %d: %s", w.Code, w.Body.String())
		}
		return p
	}

	// No schedule: nothing planned from cron, deploys still listed.
	testPool.Exec(context.Background(), `INSERT INTO server_deploy ("commit", version, started_at) VALUES ('old0000', 'v0', now() - interval '25 hours'), ('new1111', 'v1', now() - interval '2 hours'), ('new2222', 'v2', now() - interval '1 hour')`)
	p := read()
	if len(p.NextRuns) != 0 {
		t.Fatalf("next_runs without a schedule = %v, want none", p.NextRuns)
	}
	if len(p.ServerDeploy) != 2 || p.ServerDeploy[0].Commit != "new2222" || p.ServerDeploy[1].Commit != "new1111" {
		t.Fatalf("server_deploys = %+v, want the last 24 h, newest first (new2222, new1111)", p.ServerDeploy)
	}

	if w := putUpdateSchedule(t, testUserID, runtimeID, map[string]any{"enabled": true, "cron": "0 2 * * *"}); w.Code != http.StatusOK {
		t.Fatalf("set schedule = %d: %s", w.Code, w.Body.String())
	}
	p = read()
	if len(p.NextRuns) != 3 || p.Timezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("planned = %+v, want 3 next runs in Asia/Ho_Chi_Minh", p)
	}

	// A disabled schedule plans nothing.
	if w := putUpdateSchedule(t, testUserID, runtimeID, map[string]any{"enabled": false, "cron": "0 2 * * *"}); w.Code != http.StatusOK {
		t.Fatalf("disable = %d", w.Code)
	}
	if p = read(); len(p.NextRuns) != 0 {
		t.Fatalf("next_runs for a disabled schedule = %v, want none", p.NextRuns)
	}
}
