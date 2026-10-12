package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	defaultCapacityConstraint = "issue_wakeup_default_capacity"
	defaultRuleA              = "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d10"
	defaultRuleB              = "7f1d6a2e-3b44-4d8e-9a55-0c6b1f2e8d11"
)

type capacityKit struct {
	f      principalFixture
	s      *IssueWakeupService
	agent  string
	issues []string
}

func newCapacityKit(t *testing.T, issues int) capacityKit {
	t.Helper()
	f, s, issue, agent := wakeFixture(t)
	f.Cleanup(t, "DELETE FROM issue_wakeup WHERE workspace_id=$1", f.WorkspaceID)
	k := capacityKit{f: f, s: s, agent: agent, issues: []string{util.UUIDToString(issue)}}
	for len(k.issues) < issues {
		k.issues = append(k.issues, f.Issue(t, "capacity issue"))
	}
	return k
}

// seed inserts count enabled default-derived instances of rule, spread
// round-robin over the first spread issues starting at offset, so a caller can
// keep every issue under its own ceiling.
func (k capacityKit) seed(t *testing.T, scopeKind, scopeID, rule string, offset, count, spread int) {
	t.Helper()
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,default_rule_key,default_scope_kind,default_scope_id)
 SELECT gen_random_uuid(),$1,($2::uuid[])[1+(n%$3)],$4,'default','event','continuous',$5,$6,$7 FROM generate_series($8::int,$9::int) n`,
		k.f.WorkspaceID, k.issueIDs(t), spread, k.agent, rule, scopeKind, scopeID, offset, offset+count-1)
}

func (k capacityKit) issueIDs(t *testing.T) []pgtype.UUID {
	t.Helper()
	ids := make([]pgtype.UUID, len(k.issues))
	for i, id := range k.issues {
		ids[i] = parseTestUUID(t, id)
	}
	return ids
}

func (k capacityKit) insertDefault(issue, scopeKind, scopeID, rule string, enabled bool) (string, error) {
	var id string
	err := k.f.Pool.QueryRow(context.Background(), `INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,instruction,kind,mode,enabled,default_rule_key,default_scope_kind,default_scope_id)
 VALUES(gen_random_uuid(),$1,$2,$3,'default','event','continuous',$4,$5,$6,$7) RETURNING id`,
		k.f.WorkspaceID, issue, k.agent, enabled, rule, scopeKind, scopeID).Scan(&id)
	return id, err
}

func (k capacityKit) local(t *testing.T, issue string) error {
	t.Helper()
	_, err := k.s.Create(context.Background(), parseTestUUID(t, issue), parseTestUUID(t, k.f.UserID), pgtype.UUID{},
		WakeupInput{AgentID: k.agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "local"})
	return err
}

func (k capacityKit) localCount(t *testing.T) int {
	t.Helper()
	return k.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1 AND enabled AND system_rule IS NULL AND default_rule_key IS NULL`, k.f.WorkspaceID)
}

// wantDefaultFull fails unless err is the default-pool ceiling for pool.
func wantDefaultFull(t *testing.T, err error, pool string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.ConstraintName != defaultCapacityConstraint || pg.Detail != pool || pg.Message != WakeupDefaultCapacityReached {
		t.Fatalf("want default capacity ceiling %q, got %v", pool, err)
	}
}

func TestDefaultInstancesNeverConsumeLocalSlots(t *testing.T) {
	k := newCapacityKit(t, 200)
	project := uuid.NewString()
	// A custom project rule fans out to every one of 200 issues.
	k.seed(t, "project", project, defaultRuleA, 0, 200, 200)
	if got := k.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key IS NOT NULL`, k.f.WorkspaceID); got != 200 {
		t.Fatalf("default instances = %d, want 200", got)
	}
	if got := k.localCount(t); got != 0 {
		t.Fatalf("200 default instances consumed %d local slots", got)
	}
	// The issue still has its full local quota beside its default instance.
	for n := 0; n < 32; n++ {
		if err := k.local(t, k.issues[0]); err != nil {
			t.Fatalf("local wakeup %d on an issue holding a default instance: %v", n+1, err)
		}
	}
	var pg *pgconn.PgError
	if err := k.local(t, k.issues[0]); !errors.As(err, &pg) || pg.ConstraintName != "issue_wakeup_active_limit" {
		t.Fatalf("local ceiling must stay 32 per issue: %v", err)
	}
}

func TestLocalSlotsAreNotFreedByDefaultInstances(t *testing.T) {
	k := newCapacityKit(t, 1)
	scope := k.f.WorkspaceID
	for n := 0; n < 32; n++ {
		if err := k.local(t, k.issues[0]); err != nil {
			t.Fatal(err)
		}
	}
	id, err := k.insertDefault(k.issues[0], "workspace", scope, defaultRuleA, true)
	if err != nil {
		t.Fatalf("default instance beside a full local pool: %v", err)
	}
	k.f.Exec(t, `UPDATE issue_wakeup SET enabled=false,disabled_at=now() WHERE id=$1`, id)
	var pg *pgconn.PgError
	if err := k.local(t, k.issues[0]); !errors.As(err, &pg) || pg.ConstraintName != "issue_wakeup_active_limit" {
		t.Fatalf("disabling a default instance freed a local slot: %v", err)
	}
}

func TestDefaultIssueCeilingAndDeleteReenableAccounting(t *testing.T) {
	k := newCapacityKit(t, 1)
	issue, scope := k.issues[0], k.f.WorkspaceID
	var ids []string
	for n := 0; n < 32; n++ {
		id, err := k.insertDefault(issue, "workspace", scope, defaultRuleA, true)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_, err := k.insertDefault(issue, "workspace", scope, defaultRuleA, true)
	wantDefaultFull(t, err, "issue")
	// A full default pool never blocks local creation.
	if err := k.local(t, issue); err != nil {
		t.Fatalf("local wakeup blocked by a full default pool: %v", err)
	}
	// A disabled instance holds no slot; its re-enable must reacquire one.
	k.f.Exec(t, `UPDATE issue_wakeup SET enabled=false,disabled_at=now() WHERE id=$1`, ids[0])
	if _, err := k.insertDefault(issue, "workspace", scope, defaultRuleA, true); err != nil {
		t.Fatalf("slot freed by disable not reusable: %v", err)
	}
	_, err = k.f.Pool.Exec(context.Background(), `UPDATE issue_wakeup SET enabled=true,disabled_at=NULL WHERE id=$1`, ids[0])
	wantDefaultFull(t, err, "issue")
	// Deleting frees a slot too.
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE id=$1`, ids[1])
	k.f.Exec(t, `UPDATE issue_wakeup SET enabled=true,disabled_at=NULL WHERE id=$1`, ids[0])
	// An already-enabled row may be rewritten while the pool is full.
	k.f.Exec(t, `UPDATE issue_wakeup SET instruction='edited' WHERE id=$1`, ids[0])
}

func TestDefaultScopeCeilingIsSharedByRulesAndLeavesLocalFree(t *testing.T) {
	k := newCapacityKit(t, 40)
	project := uuid.NewString()
	// Two root rules of one project share the 1,000 ceiling.
	k.seed(t, "project", project, defaultRuleA, 0, 600, 40)
	k.seed(t, "project", project, defaultRuleB, 600, 399, 40)
	if _, err := k.insertDefault(k.issues[39], "project", project, defaultRuleB, true); err != nil {
		t.Fatalf("1000th scope slot: %v", err)
	}
	_, err := k.insertDefault(k.issues[38], "project", project, defaultRuleA, true)
	wantDefaultFull(t, err, "scope")
	// Another scope, and every local rule, are unaffected.
	if _, err := k.insertDefault(k.issues[38], "project", uuid.NewString(), defaultRuleA, true); err != nil {
		t.Fatalf("other scope blocked: %v", err)
	}
	if err := k.local(t, k.issues[0]); err != nil {
		t.Fatalf("eligible local rule blocked by a full default scope: %v", err)
	}
}

func TestDefaultWorkspaceCeilingAndConcurrentAdmission(t *testing.T) {
	k := newCapacityKit(t, 160)
	var scopes []string
	for n := 0; n < 6; n++ {
		scopes = append(scopes, uuid.NewString())
	}
	for n := 0; n < 4; n++ {
		k.seed(t, "project", scopes[n], defaultRuleA, n*1000, 1000, 160)
	}
	k.seed(t, "project", scopes[4], defaultRuleA, 4000, 999, 160) // 4,999 of 5,000
	// One workspace slot left; two different scopes and issues race for it.
	k.raceDefault(t, "workspace", [2]string{k.issues[158], k.issues[159]}, [2]string{scopes[4], scopes[5]})
	if got := k.f.Count(t, `SELECT count(*) FROM issue_wakeup WHERE workspace_id=$1 AND default_rule_key IS NOT NULL AND enabled`, k.f.WorkspaceID); got != 5000 {
		t.Fatalf("default instances = %d, want 5000", got)
	}
	if err := k.local(t, k.issues[0]); err != nil {
		t.Fatalf("local rule blocked by a full workspace default pool: %v", err)
	}
	// Built-in identities keep their exemption, overridden or not.
	k.f.Exec(t, `INSERT INTO issue_wakeup(id,workspace_id,issue_id,instruction,kind,mode,system_rule,default_rule_key,default_scope_kind,default_scope_id)
 VALUES(gen_random_uuid(),$1,$2,'built-in','event','continuous','pr_merged','pr_merged','workspace',$1)`, k.f.WorkspaceID, k.issues[2])
}

// raceDefault inserts one default instance per target at once and requires
// that exactly one wins and the loser names pool.
func (k capacityKit) raceDefault(t *testing.T, pool string, targets [2]string, scopes [2]string) {
	t.Helper()
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = k.insertDefault(targets[i], "project", scopes[i], defaultRuleA, true)
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("%s ceiling raced past: %v / %v", pool, errs[0], errs[1])
	}
	for _, err := range errs {
		if err != nil {
			wantDefaultFull(t, err, pool)
		}
	}
}

func TestDefaultIssueCeilingAdmitsConcurrentWritersOnce(t *testing.T) {
	k := newCapacityKit(t, 1)
	scope := uuid.NewString()
	k.seed(t, "project", scope, defaultRuleA, 0, 31, 1)
	k.raceDefault(t, "issue", [2]string{k.issues[0], k.issues[0]}, [2]string{scope, scope})
}

func TestDefaultScopeCeilingAdmitsConcurrentWritersOnce(t *testing.T) {
	k := newCapacityKit(t, 40)
	scope := uuid.NewString()
	k.seed(t, "project", scope, defaultRuleA, 0, 999, 40)
	k.raceDefault(t, "scope", [2]string{k.issues[0], k.issues[1]}, [2]string{scope, scope})
}

func TestDefaultCapacityReasonLeavesInstanceUnappliedAndSourceWritesWork(t *testing.T) {
	k := newCapacityKit(t, 1)
	issue, scope := k.issues[0], k.f.WorkspaceID
	var enabled []string
	for n := 0; n < 32; n++ {
		id, err := k.insertDefault(issue, "workspace", scope, defaultRuleA, true)
		if err != nil {
			t.Fatal(err)
		}
		enabled = append(enabled, id)
	}
	waiting, err := k.insertDefault(issue, "workspace", scope, defaultRuleA, false)
	if err != nil {
		t.Fatal(err)
	}
	apply := func() DefaultWakeupAdmission {
		t.Helper()
		tx, err := k.f.Pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		got, err := ApplyDefaultWakeupInstance(context.Background(), tx, parseTestUUID(t, waiting))
		if err != nil {
			t.Fatalf("a full default pool must not fail the caller's transaction: %v", err)
		}
		if err := tx.Commit(context.Background()); err != nil {
			t.Fatalf("caller transaction poisoned by the capacity refusal: %v", err)
		}
		return got
	}
	got := apply()
	if got.Applied || got.Pool != "issue" || got.Reason != "Default capacity reached" {
		t.Fatalf("admission = %+v", got)
	}
	var w db.IssueWakeup
	w, err = k.f.q.GetIssueWakeup(context.Background(), db.GetIssueWakeupParams{ID: parseTestUUID(t, waiting), WorkspaceID: parseTestUUID(t, k.f.WorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if w.Enabled || w.CapacityReason.String != "default" || w.LastError.String != "Default capacity reached" {
		t.Fatalf("unapplied instance state: enabled=%v reason=%q last_error=%q", w.Enabled, w.CapacityReason.String, w.LastError.String)
	}
	// Source writes and local-rule creation succeed beside the full pool.
	k.f.Comment(t, issue, "source write")
	if err := k.local(t, issue); err != nil {
		t.Fatalf("local creation: %v", err)
	}
	// Freeing a slot lets the next application succeed and clears the reason.
	k.f.Exec(t, `DELETE FROM issue_wakeup WHERE id=$1`, enabled[0])
	if got := apply(); !got.Applied || got.Reason != "" {
		t.Fatalf("admission after a slot freed = %+v", got)
	}
	w, err = k.f.q.GetIssueWakeup(context.Background(), db.GetIssueWakeupParams{ID: parseTestUUID(t, waiting), WorkspaceID: parseTestUUID(t, k.f.WorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.CapacityReason.Valid || w.LastError.Valid {
		t.Fatalf("applied instance kept a stale reason: %+v", w)
	}
}

func TestWakeupInputCannotCarryOriginFields(t *testing.T) {
	k := newCapacityKit(t, 1)
	w := wakeCreate(t, k.f, k.s, parseTestUUID(t, k.issues[0]), WakeupInput{AgentID: k.agent, Kind: "event", EventTypes: []string{"comment.created"}, Instruction: "local"})
	if w.DefaultRuleKey.Valid || w.DefaultScopeKind.Valid || w.DefaultScopeID.Valid || w.CapacityReason.Valid {
		t.Fatalf("a locally created wakeup carries origin metadata: %+v", w)
	}
	disabled, err := k.s.Disable(context.Background(), parseTestUUID(t, k.issues[0]), w.ID, parseTestUUID(t, k.f.UserID))
	if err != nil {
		t.Fatal(err)
	}
	re, err := k.s.Enable(context.Background(), parseTestUUID(t, k.issues[0]), parseTestUUID(t, k.f.UserID), pgtype.UUID{}, w.ID, WakeupEnableInput{Revision: disabled.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if re.DefaultRuleKey.Valid || re.DefaultScopeKind.Valid || re.DefaultScopeID.Valid {
		t.Fatalf("a local update gave the row an origin: %+v", re)
	}
}
