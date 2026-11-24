package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CHE-7408 (B1 of CHE-7148): additive storage for the dequeue-cause record.
const mergeQueueTablesVersion = "999q_merge_queue_tables"

var mergeQueueTables = []string{
	"merge_queue_intake", "merge_queue_epoch", "merge_queue_association", "merge_queue_wait",
	"merge_queue_pr_close", "merge_queue_enqueue_event", "merge_queue_state", "merge_queue_evidence",
}

// version -> index, in migration order. Each is one concurrent unique build.
var mergeQueueIndexes = []struct{ version, index, table string }{
	{"999r_merge_queue_intake_removal_uidx", "merge_queue_intake_removal_uidx", "merge_queue_intake"},
	{"999s_merge_queue_epoch_key_uidx", "merge_queue_epoch_key_uidx", "merge_queue_epoch"},
	{"999t_merge_queue_association_active_uidx", "merge_queue_association_active_uidx", "merge_queue_association"},
	{"999u_merge_queue_wait_unmet_uidx", "merge_queue_wait_unmet_uidx", "merge_queue_wait"},
	{"999v_merge_queue_pr_close_uidx", "merge_queue_pr_close_uidx", "merge_queue_pr_close"},
	{"999w_merge_queue_enqueue_event_node_uidx", "merge_queue_enqueue_event_node_uidx", "merge_queue_enqueue_event"},
	{"999x_merge_queue_state_pr_uidx", "merge_queue_state_pr_uidx", "merge_queue_state"},
	// CHE-7409: the removal node id is unique per workspace.
	{"99h_merge_queue_intake_ws_removal_uidx", "merge_queue_intake_ws_removal_uidx", "merge_queue_intake"},
}

// CHE-7409: retires the global removal-node index. Its up is one concurrent
// drop; its down rebuilds the 999r index.
const mergeQueueDropGlobalRemovalVersion = "99i_merge_queue_intake_drop_global_removal_uidx"

func TestMergeQueueMigrationsFollowTheRules(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"up", "down"} {
		body := strings.ToUpper(stripSQLComments(readWakeupMigration(t, mergeQueueTablesVersion, direction)))
		for _, banned := range []string{"REFERENCES", "FOREIGN KEY", "ON DELETE", "ON UPDATE", "CREATE INDEX", "CREATE UNIQUE", "UNIQUE (", "CREATE FUNCTION", "CREATE TRIGGER", "ALTER TABLE"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s.%s contains %q", mergeQueueTablesVersion, direction, banned)
			}
		}
	}
	down := stripSQLComments(readWakeupMigration(t, mergeQueueTablesVersion, "down"))
	if got := strings.Count(strings.ToUpper(down), "DROP TABLE IF EXISTS"); got != len(mergeQueueTables) {
		t.Errorf("down drops %d tables, want %d", got, len(mergeQueueTables))
	}
	for _, table := range mergeQueueTables {
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+table+";") {
			t.Errorf("down does not drop %s", table)
		}
	}
	for _, ix := range mergeQueueIndexes {
		if got := concurrentIndexCleanups[ix.version]; got != ix.index {
			t.Errorf("%s cleanup index = %q, want %q", ix.version, got, ix.index)
		}
		req, ok := requiredConcurrentIndexes[ix.version]
		if !ok || req.IndexRegclass != ix.index || req.TableRegclass != ix.table || !req.Unique {
			t.Errorf("%s validity requirement = %+v", ix.version, req)
		}
		up := stripSQLComments(readWakeupMigration(t, ix.version, "up"))
		if strings.Count(up, ";") != 1 || !strings.Contains(up, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "+ix.index) {
			t.Errorf("%s up must be one concurrent re-runnable build:\n%s", ix.version, up)
		}
		dn := stripSQLComments(readWakeupMigration(t, ix.version, "down"))
		if strings.Count(dn, ";") != 1 || !strings.Contains(dn, "DROP INDEX CONCURRENTLY IF EXISTS "+ix.index) {
			t.Errorf("%s down must be one concurrent drop:\n%s", ix.version, dn)
		}
	}
}

func TestMergeQueueDropsTheGlobalRemovalIndexInOneStatement(t *testing.T) {
	t.Parallel()
	up := stripSQLComments(readWakeupMigration(t, mergeQueueDropGlobalRemovalVersion, "up"))
	if strings.Count(up, ";") != 1 || !strings.Contains(up, "DROP INDEX CONCURRENTLY IF EXISTS merge_queue_intake_removal_uidx") {
		t.Errorf("up must be one concurrent drop of the global index:\n%s", up)
	}
	dn := stripSQLComments(readWakeupMigration(t, mergeQueueDropGlobalRemovalVersion, "down"))
	if strings.Count(dn, ";") != 1 || !strings.Contains(dn, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_intake_removal_uidx") {
		t.Errorf("down must be one concurrent rebuild of the global index:\n%s", dn)
	}
	if got := concurrentDownIndexCleanups[mergeQueueDropGlobalRemovalVersion]; got != "merge_queue_intake_removal_uidx" {
		t.Errorf("down cleanup = %q, want merge_queue_intake_removal_uidx", got)
	}
}

func TestMergeQueueSchemaUpDownUpAndKeys(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "merge_queue_")
	pool := openTestPoolWithSearchPath(t, schema)

	apply := func(version, direction string) {
		t.Helper()
		if _, err := pool.Exec(ctx, readWakeupMigration(t, version, direction)); err != nil {
			t.Fatalf("%s %s: %v", version, direction, err)
		}
	}
	up := func() {
		apply(mergeQueueTablesVersion, "up")
		for _, ix := range mergeQueueIndexes {
			apply(ix.version, "up")
		}
		apply(mergeQueueDropGlobalRemovalVersion, "up")
	}
	down := func() {
		apply(mergeQueueDropGlobalRemovalVersion, "down")
		for i := len(mergeQueueIndexes) - 1; i >= 0; i-- {
			apply(mergeQueueIndexes[i].version, "down")
		}
		apply(mergeQueueTablesVersion, "down")
	}
	tables := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename LIKE 'merge\_queue\_%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	up()
	if got := tables(); got != len(mergeQueueTables) {
		t.Fatalf("tables after up = %d, want %d", got, len(mergeQueueTables))
	}
	for _, ix := range mergeQueueIndexes { // re-running an up file is harmless
		apply(ix.version, "up")
	}
	apply(mergeQueueDropGlobalRemovalVersion, "up")

	const ws = "11111111-1111-1111-1111-111111111111"
	const issue = "22222222-2222-2222-2222-222222222222"
	exec := func(sql string) error { _, err := pool.Exec(ctx, sql); return err }
	dup := func(name, first, second string) {
		t.Helper()
		if err := exec(first); err != nil {
			t.Fatalf("%s: first insert: %v", name, err)
		}
		if err := exec(second); err == nil || !strings.Contains(err.Error(), "23505") {
			t.Errorf("%s: duplicate insert error = %v, want unique violation", name, err)
		}
	}
	refused := func(name, sql string) {
		t.Helper()
		if err := exec(sql); err == nil || !strings.Contains(err.Error(), "23514") {
			t.Errorf("%s: error = %v, want check violation", name, err)
		}
	}
	pr := `workspace_id, repo_owner, repo_name, pr_number`
	prv := `'` + ws + `', 'o', 'r', 7`

	// Unique keys.
	dup("intake removal node", `INSERT INTO merge_queue_intake (`+pr+`, removal_node_id) VALUES (`+prv+`, 'RM1')`, `INSERT INTO merge_queue_intake (`+pr+`, removal_node_id) VALUES (`+prv+`, 'RM1')`)
	if err := exec(`INSERT INTO merge_queue_intake (workspace_id, repo_owner, repo_name, pr_number, removal_node_id) VALUES ('33333333-3333-3333-3333-333333333333', 'o', 'r', 7, 'RM1')`); err != nil {
		t.Errorf("the same removal node in another workspace must be allowed: %v", err)
	}
	if err := exec(`INSERT INTO merge_queue_intake (` + pr + `) VALUES (` + prv + `), (` + prv + `)`); err != nil {
		t.Errorf("intake rows not yet bound to a removal node must coexist: %v", err)
	}
	dup("epoch key", `INSERT INTO merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, epoch_no, kind) VALUES ('`+ws+`', 'o', 'r', 'k', 1, 'shared')`, `INSERT INTO merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, epoch_no, kind) VALUES ('`+ws+`', 'o', 'r', 'k', 1, 'suspected')`)
	dup("active association", `INSERT INTO merge_queue_association (`+pr+`, cause_key, started_at) VALUES (`+prv+`, 'k', now())`, `INSERT INTO merge_queue_association (`+pr+`, cause_key, started_at) VALUES (`+prv+`, 'k', now())`)
	if err := exec(`UPDATE merge_queue_association SET ended_at = now(), end_reason = 'pr_closed'`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO merge_queue_association (` + pr + `, cause_key, started_at) VALUES (` + prv + `, 'k', now())`); err != nil {
		t.Errorf("a new association after an ended one must be allowed: %v", err)
	}
	dup("unmet wait", `INSERT INTO merge_queue_wait (`+pr+`, issue_id, kind) VALUES (`+prv+`, '`+issue+`', 'wait')`, `INSERT INTO merge_queue_wait (`+pr+`, issue_id, kind) VALUES (`+prv+`, '`+issue+`', 'would_wait')`)
	if err := exec(`UPDATE merge_queue_wait SET state = 'met'`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO merge_queue_wait (` + pr + `, issue_id, kind) VALUES (` + prv + `, '` + issue + `', 'wait')`); err != nil {
		t.Errorf("a new wait after a met one must be allowed: %v", err)
	}
	dup("pr close", `INSERT INTO merge_queue_pr_close (`+pr+`, closed_at) VALUES (`+prv+`, '2026-11-18 10:00:00+00')`, `INSERT INTO merge_queue_pr_close (`+pr+`, closed_at) VALUES (`+prv+`, '2026-11-18 10:00:00+00')`)
	if err := exec(`INSERT INTO merge_queue_pr_close (` + pr + `, closed_at) VALUES (` + prv + `, '2026-11-18 11:00:00+00')`); err != nil {
		t.Errorf("a second close of the same PR at another time must be allowed: %v", err)
	}
	dup("enqueue node id", `INSERT INTO merge_queue_enqueue_event (`+pr+`, node_id, node_created_at) VALUES (`+prv+`, 'N1', now())`, `INSERT INTO merge_queue_enqueue_event (workspace_id, repo_owner, repo_name, pr_number, node_id, node_created_at) VALUES ('`+ws+`', 'o', 'r', 8, 'N1', now())`)
	dup("queue state", `INSERT INTO merge_queue_state (`+pr+`, state) VALUES (`+prv+`, 'enqueued')`, `INSERT INTO merge_queue_state (`+pr+`, state) VALUES (`+prv+`, 'dequeued')`)
	if err := exec(`INSERT INTO merge_queue_evidence (` + pr + `, kind) VALUES (` + prv + `, 'main_run'), (` + prv + `, 'main_run')`); err != nil {
		t.Errorf("evidence rows are append-only and may repeat: %v", err)
	}

	// Defaults and CHECK constraints on every text state column.
	var delivery, routing string
	if err := pool.QueryRow(ctx, `SELECT delivery, routing FROM merge_queue_intake LIMIT 1`).Scan(&delivery, &routing); err != nil || delivery != "pending" || routing != "warn" {
		t.Errorf("intake defaults = %q/%q (err %v), want pending/warn", delivery, routing, err)
	}
	var epochState string
	if err := pool.QueryRow(ctx, `SELECT state FROM merge_queue_epoch`).Scan(&epochState); err != nil || epochState != "open" {
		t.Errorf("epoch default state = %q (err %v), want open", epochState, err)
	}
	refused("intake delivery", `INSERT INTO merge_queue_intake (`+pr+`, delivery) VALUES (`+prv+`, 'sent')`)
	refused("intake routing", `INSERT INTO merge_queue_intake (`+pr+`, routing) VALUES (`+prv+`, 'bogus')`)
	refused("intake status", `INSERT INTO merge_queue_intake (`+pr+`, status) VALUES (`+prv+`, 'bogus')`)
	refused("epoch state", `INSERT INTO merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, epoch_no, kind, state) VALUES ('`+ws+`', 'o', 'r', 'k2', 1, 'shared', 'done')`)
	refused("epoch kind", `INSERT INTO merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, epoch_no, kind) VALUES ('`+ws+`', 'o', 'r', 'k3', 1, 'local')`)
	refused("epoch no", `INSERT INTO merge_queue_epoch (workspace_id, repo_owner, repo_name, cause_key, epoch_no, kind) VALUES ('`+ws+`', 'o', 'r', 'k4', 0, 'shared')`)
	refused("association end reason", `INSERT INTO merge_queue_association (`+pr+`, cause_key, started_at, end_reason) VALUES (`+prv+`, 'k9', now(), 'enqueued')`)
	refused("wait kind", `INSERT INTO merge_queue_wait (`+pr+`, issue_id, kind) VALUES (`+prv+`, gen_random_uuid(), 'hold')`)
	refused("wait state", `INSERT INTO merge_queue_wait (`+pr+`, issue_id, kind, state) VALUES (`+prv+`, gen_random_uuid(), 'wait', 'open')`)
	refused("enqueue actor class", `INSERT INTO merge_queue_enqueue_event (`+pr+`, node_id, node_created_at, actor_class) VALUES (`+prv+`, 'N2', now(), 'bot')`)
	refused("enqueue head state", `INSERT INTO merge_queue_enqueue_event (`+pr+`, node_id, node_created_at, head_state) VALUES (`+prv+`, 'N3', now(), 'maybe')`)
	refused("enqueue label", `INSERT INTO merge_queue_enqueue_event (`+pr+`, node_id, node_created_at, label) VALUES (`+prv+`, 'N4', now(), 'third')`)
	refused("queue state", `INSERT INTO merge_queue_state (workspace_id, repo_owner, repo_name, pr_number, state) VALUES ('`+ws+`', 'o', 'r', 9, 'merged')`)
	refused("evidence kind", `INSERT INTO merge_queue_evidence (`+pr+`, kind) VALUES (`+prv+`, 'guess')`)
	for _, label := range []string{"first", "requeue-while-open", "requeue-after-close", "second-requeue-same-head", "new-head", "requeue-order-unknown", "requeue-head-unknown"} {
		if err := exec(`INSERT INTO merge_queue_enqueue_event (` + pr + `, node_id, node_created_at, label) VALUES (` + prv + `, 'L-` + label + `', now(), '` + label + `')`); err != nil {
			t.Errorf("label %q refused: %v", label, err)
		}
	}

	// Down drops these tables only and up works again.
	if err := exec(`CREATE TABLE merge_queue_bystander_probe (id int)`); err != nil {
		t.Fatal(err)
	}
	// The 99i down restores the global index, which a removal node shared by two
	// workspaces would violate; a rollback is expected to fail loudly then.
	if err := exec(`DELETE FROM merge_queue_intake WHERE workspace_id = '33333333-3333-3333-3333-333333333333'`); err != nil {
		t.Fatal(err)
	}
	down()
	if got := tables(); got != 1 {
		t.Fatalf("tables after down = %d, want 1 (the bystander probe)", got)
	}
	if err := exec(`DROP TABLE merge_queue_bystander_probe`); err != nil {
		t.Fatal(err)
	}
	up()
	if got := tables(); got != len(mergeQueueTables) {
		t.Fatalf("tables after up-down-up = %d, want %d", got, len(mergeQueueTables))
	}
}
