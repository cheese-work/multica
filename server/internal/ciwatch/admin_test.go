package ciwatch

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func parse(t *testing.T, body string) (Patch, error) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	return ParsePatch(raw)
}

// Every field of the settings panel has a rejecting case and an accepting case.
func TestParsePatch_ValidatesEveryField(t *testing.T) {
	long := strings.Repeat("x", 201)
	many := `["` + strings.Repeat(`a","`, 60) + `a"]`
	id := uuid.NewString()
	cases := []struct {
		name, body, badField string // badField "" = must be accepted
	}{
		{"mode off", `{"mode":"off"}`, ""},
		{"mode act", `{"mode":"act"}`, ""},
		{"mode bogus", `{"mode":"loud"}`, "mode"},
		{"mode wrong type", `{"mode":1}`, "mode"},
		{"mode null", `{"mode":null}`, "mode"},
		{"p1 shadow", `{"p1_mode":"shadow"}`, ""},
		{"p1 off is not a protocol mode", `{"p1_mode":"off"}`, "p1_mode"},
		{"p2 bogus", `{"p2_mode":"x"}`, "p2_mode"},
		{"p3 bogus", `{"p3_mode":""}`, "p3_mode"},
		{"p4 bogus", `{"p4_mode":"ACT"}`, "p4_mode"},
		{"branches ok", `{"branches":["main","release/*"]}`, ""},
		{"branches empty list ok", `{"branches":[]}`, ""},
		{"branches blank entry", `{"branches":["main",""]}`, "branches"},
		{"branches too long entry", `{"branches":["` + long + `"]}`, "branches"},
		{"branches too many", `{"branches":` + many + `}`, "branches"},
		{"branches not list", `{"branches":"main"}`, "branches"},
		{"branches null", `{"branches":null}`, "branches"},
		{"branches control char", `{"branches":["a\nb"]}`, "branches"},
		{"events ok", `{"events":["push","workflow_run"]}`, ""},
		{"events upper", `{"events":["Push"]}`, "events"},
		{"events blank", `{"events":[""]}`, "events"},
		{"include ok", `{"workflows_include":[".github/workflows/ci.yml"]}`, ""},
		{"include blank", `{"workflows_include":[" "]}`, "workflows_include"},
		{"exclude ok", `{"workflows_exclude":["nightly*"]}`, ""},
		{"exclude not list", `{"workflows_exclude":3}`, "workflows_exclude"},
		{"owner squad", `{"fix_owner_kind":"squad","fix_owner_id":"` + id + `"}`, ""},
		{"owner cleared", `{"fix_owner_kind":null,"fix_owner_id":null}`, ""},
		{"owner kind alone", `{"fix_owner_kind":"squad"}`, "fix_owner_id"},
		{"owner id alone", `{"fix_owner_id":"` + id + `"}`, "fix_owner_kind"},
		{"owner kind set id null", `{"fix_owner_kind":"squad","fix_owner_id":null}`, "fix_owner_id"},
		{"owner kind bogus", `{"fix_owner_kind":"team","fix_owner_id":"` + id + `"}`, "fix_owner_kind"},
		{"owner id not uuid", `{"fix_owner_kind":"agent","fix_owner_id":"nope"}`, "fix_owner_id"},
		{"notify ok", `{"notify_member_id":"` + id + `"}`, ""},
		{"notify cleared", `{"notify_member_id":null}`, ""},
		{"notify not uuid", `{"notify_member_id":"x"}`, "notify_member_id"},
		{"reruns zero", `{"max_reruns_per_original_run":0}`, ""},
		{"reruns negative", `{"max_reruns_per_original_run":-1}`, "max_reruns_per_original_run"},
		{"reruns fraction", `{"max_reruns_per_original_run":1.5}`, "max_reruns_per_original_run"},
		{"reruns string", `{"max_reruns_per_original_run":"2"}`, "max_reruns_per_original_run"},
		{"reruns null", `{"max_reruns_per_original_run":null}`, "max_reruns_per_original_run"},
		{"issues null", `{"max_new_issues_per_day":null}`, "max_new_issues_per_day"},
		{"wakes null", `{"max_owner_wakes_per_hour":null}`, "max_owner_wakes_per_hour"},
		{"issues ok", `{"max_new_issues_per_day":10}`, ""},
		{"issues negative", `{"max_new_issues_per_day":-5}`, "max_new_issues_per_day"},
		{"issues huge", `{"max_new_issues_per_day":100001}`, "max_new_issues_per_day"},
		{"wakes ok", `{"max_owner_wakes_per_hour":3}`, ""},
		{"wakes negative", `{"max_owner_wakes_per_hour":-1}`, "max_owner_wakes_per_hour"},
		{"unknown field", `{"repo":"a/b"}`, "repo"},
		{"unknown field 2", `{"nope":1}`, "nope"},
		{"empty patch", `{}`, "body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.body)
			if tc.badField == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			var ve *ValidationError
			if err == nil || !asValidation(err, &ve) {
				t.Fatalf("want ValidationError on %q, got %v", tc.badField, err)
			}
			if ve.Field != tc.badField {
				t.Fatalf("field = %q, want %q (%v)", ve.Field, tc.badField, err)
			}
		})
	}
}

func asValidation(err error, target **ValidationError) bool {
	ve, ok := err.(*ValidationError)
	*target = ve
	return ok
}

func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{"Cheese-Work/Multica": "cheese-work/multica", "a.b/c_d": "a.b/c_d"} {
		o, n, err := ParseRepo(in)
		if err != nil || o+"/"+n != want {
			t.Errorf("ParseRepo(%q) = %q/%q, %v; want %s", in, o, n, err, want)
		}
	}
	for _, in := range []string{"", "a", "a/", "/b", "a/b/c", "a b/c", "a/b;drop", strings.Repeat("x", 101) + "/y"} {
		if _, _, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) accepted", in)
		}
	}
}

type adminEnv struct {
	pool        *pgxpool.Pool
	ws, actor   string
	owner, name string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	e := &adminEnv{pool: pool, ws: uuid.NewString(), actor: uuid.NewString(), owner: "o", name: "r" + uuid.NewString()[:8]}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"ci_watch_config", "ci_watch_config_audit", "ci_watch_action", "ci_failure_event", "ci_watch_event"} {
			_, _ = pool.Exec(ctx, `DELETE FROM `+tbl+` WHERE workspace_id=$1`, e.ws)
		}
		pool.Close()
	})
	return e
}

func (e *adminEnv) apply(t *testing.T, body string) ConfigResult {
	t.Helper()
	p, err := parse(t, body)
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	res, err := ApplyConfig(context.Background(), e.pool, e.ws, e.owner, e.name, e.actor, p)
	if err != nil {
		t.Fatalf("apply %s: %v", body, err)
	}
	return res
}

func (e *adminEnv) action(t *testing.T, run int64, protocol string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO ci_watch_action
		(workspace_id, repo_owner, repo_name, original_run_id, action_kind, protocol, state)
		VALUES ($1,$2,$3,$4,'issue_create',$5,'scheduled')`, e.ws, e.owner, e.name, run, protocol); err != nil {
		t.Fatal(err)
	}
}

func (e *adminEnv) states(t *testing.T) map[string]string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT protocol, state||':'||reason FROM ci_watch_action WHERE workspace_id=$1`, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, s string
		if err := rows.Scan(&p, &s); err != nil {
			t.Fatal(err)
		}
		out[p] = s
	}
	return out
}

type auditRow struct{ Field, Old, New string }

func (e *adminEnv) audit(t *testing.T) []auditRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT field, COALESCE(old_value,'<null>'), COALESCE(new_value,'<null>')
		FROM ci_watch_config_audit WHERE workspace_id=$1 AND actor_id=$2 AND actor_type='member' ORDER BY field`, e.ws, e.actor)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Field, &r.Old, &r.New); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestApplyConfig_AuditRowPerChangedField(t *testing.T) {
	e := newAdminEnv(t)
	res := e.apply(t, `{"mode":"shadow","branches":["main","dev"],"max_new_issues_per_day":9,"p2_mode":"shadow"}`)
	// p2_mode is already shadow by default: not a change, no audit row.
	want := []auditRow{
		{"branches", "[]", `["main","dev"]`},
		{"max_new_issues_per_day", "5", "9"},
		{"mode", "off", "shadow"},
	}
	if got := e.audit(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit rows = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(res.Changed, []string{"branches", "max_new_issues_per_day", "mode"}) {
		t.Fatalf("Changed = %v", res.Changed)
	}
	if res.Config.Mode != "shadow" || res.Config.MaxNewIssuesPerDay != 9 || !reflect.DeepEqual(res.Config.Branches, []string{"main", "dev"}) {
		t.Fatalf("config = %+v", res.Config)
	}

	// Same values again: nothing changes, nothing is written.
	again := e.apply(t, `{"mode":"shadow","branches":["main","dev"]}`)
	if len(again.Changed) != 0 || len(e.audit(t)) != 3 {
		t.Fatalf("no-op patch changed %v, audit now %d rows", again.Changed, len(e.audit(t)))
	}

	// Nullable fields audit as NULL.
	id := uuid.NewString()
	e.apply(t, `{"notify_member_id":"`+id+`"}`)
	e.apply(t, `{"notify_member_id":null}`)
	var got []string
	for _, r := range e.audit(t) {
		if r.Field == "notify_member_id" {
			got = append(got, r.Old+"->"+r.New)
		}
	}
	if len(got) != 2 {
		t.Fatalf("notify audit = %v", got)
	}
}

func TestApplyConfig_SwitchToOffOrShadowCancelsPending(t *testing.T) {
	for _, target := range []string{"off", "shadow"} {
		t.Run(target, func(t *testing.T) {
			e := newAdminEnv(t)
			e.apply(t, `{"mode":"act"}`)
			e.action(t, 1, "P1")
			e.action(t, 2, "P2")
			res := e.apply(t, `{"mode":"`+target+`"}`)
			if res.Cancelled != 2 {
				t.Fatalf("Cancelled = %d, want 2", res.Cancelled)
			}
			for p, s := range e.states(t) {
				if s != "cancelled:opted_out" {
					t.Errorf("%s action = %s, want cancelled:opted_out", p, s)
				}
			}
		})
	}
}

func TestApplyConfig_OtherChangesKeepPending(t *testing.T) {
	e := newAdminEnv(t)
	e.apply(t, `{"mode":"shadow"}`)
	e.action(t, 1, "P1")
	// Off -> shadow-to-act, and unrelated fields, cancel nothing.
	if res := e.apply(t, `{"mode":"act","branches":["main"],"p1_mode":"act"}`); res.Cancelled != 0 {
		t.Fatalf("Cancelled = %d, want 0", res.Cancelled)
	}
	// A repeat Off->Off is not a switch.
	e.apply(t, `{"mode":"off"}`) // cancels the P1 row once
	e.action(t, 2, "P1")
	if res := e.apply(t, `{"mode":"off"}`); res.Cancelled != 0 {
		t.Fatalf("repeat off cancelled %d", res.Cancelled)
	}
	if s := e.states(t)["P1"]; s != "scheduled:" && s != "cancelled:opted_out" {
		t.Fatalf("state = %s", s)
	}
}

func TestApplyConfig_ProtocolToShadowCancelsOnlyThatProtocol(t *testing.T) {
	e := newAdminEnv(t)
	e.apply(t, `{"mode":"act","p1_mode":"act","p2_mode":"act"}`)
	e.action(t, 1, "P1")
	e.action(t, 2, "P2")
	res := e.apply(t, `{"p1_mode":"shadow"}`)
	if res.Cancelled != 1 {
		t.Fatalf("Cancelled = %d, want 1", res.Cancelled)
	}
	st := e.states(t)
	if st["P1"] != "cancelled:opted_out" || st["P2"] != "scheduled:" {
		t.Fatalf("states = %v", st)
	}
}

func TestDefaultConfig_MatchesColumnDefaults(t *testing.T) {
	e := newAdminEnv(t)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name) VALUES ($1,$2,$3)`, e.ws, e.owner, e.name); err != nil {
		t.Fatal(err)
	}
	got, err := GetConfig(context.Background(), e.pool, e.ws, e.owner, e.name)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt == nil {
		t.Fatal("a stored row reports updated_at")
	}
	got.UpdatedAt = nil
	want := DefaultConfig(e.owner + "/" + e.name)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored defaults %+v\n!= DefaultConfig %+v", got, want)
	}
	// A repo with no row reads as the defaults, and is not created by reading.
	other, err := GetConfig(context.Background(), e.pool, e.ws, e.owner, "missing")
	if err != nil || other.UpdatedAt != nil || other.Mode != "off" {
		t.Fatalf("missing repo = %+v, %v", other, err)
	}
}

func TestListEvents_RepoScopedNewestFirstLimited(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := e.pool.Exec(ctx, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, workflow_name, head_branch, created_at)
			VALUES ($1,$2,$3,$4,1,'CI','main', now() - make_interval(mins => $5))`, e.ws, e.owner, e.name, 100+i, 10-i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_watch_event (workspace_id, repo_owner, repo_name, kind, detail, created_at)
		VALUES ($1,$2,$3,'breaker_trip','paused', now())`, e.ws, e.owner, e.name); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt)
		VALUES ($1,$2,'other',999,1)`, e.ws, e.owner); err != nil {
		t.Fatal(err)
	}
	all, err := ListEvents(ctx, e.pool, e.ws, e.owner, e.name, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].Kind != "breaker_trip" || all[1].RunID != 103 || all[3].RunID != 101 {
		t.Fatalf("events = %+v", all)
	}
	if all[1].Kind != "ci_failure" || all[1].Workflow != "CI" || all[1].Branch != "main" {
		t.Fatalf("failure event = %+v", all[1])
	}
	two, err := ListEvents(ctx, e.pool, e.ws, e.owner, e.name, 2)
	if err != nil || len(two) != 2 {
		t.Fatalf("limit 2 -> %d rows, %v", len(two), err)
	}
}

func TestListEvents_EnrichedWithClassProtocolActionOutcome(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, workflow_name, head_branch, event, conclusion)
		VALUES ($1,$2,$3,201,2,'CI','main','push','failure'), ($1,$2,$3,202,1,'CI','main','push','failure')`, e.ws, e.owner, e.name); err != nil {
		t.Fatal(err)
	}
	subj := subjectID(Run{RepoOwner: e.owner, RepoName: e.name, RunID: 201, RunAttempt: 2})
	if _, err := e.pool.Exec(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, choice_key, outcome_label)
		VALUES ($1,'ci_event',$2,'flake','P1','rerun_green')`, e.ws, subj); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind, state, protocol, reason)
		VALUES ($1,$2,$3,201,'rerun','done','P1','')`, e.ws, e.owner, e.name); err != nil {
		t.Fatal(err)
	}
	got, err := ListEvents(ctx, e.pool, e.ws, e.owner, e.name, 50)
	if err != nil || len(got) != 2 {
		t.Fatalf("events = %+v, %v", got, err)
	}
	byRun := map[int64]WatchEvent{got[0].RunID: got[0], got[1].RunID: got[1]}
	r := byRun[201]
	if r.Event != "push" || r.Class != "flake" || r.Protocol != "P1" || r.Action != "rerun" || r.State != "done" || r.Outcome != "rerun_green" {
		t.Fatalf("enriched event = %+v", r)
	}
	if bare := byRun[202]; bare.Event != "push" || bare.Class != "" || bare.Action != "" || bare.Outcome != "" {
		t.Fatalf("event without observation/action = %+v", bare)
	}
}

func TestListEvents_WouldDoIsRoutedProtocolUnlessActed(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt, workflow_name, head_branch, event, conclusion)
		VALUES ($1,$2,$3,301,1,'CI','main','push','failure'), ($1,$2,$3,302,1,'CI','main','push','failure'), ($1,$2,$3,303,1,'CI','main','push','failure'), ($1,$2,$3,304,1,'CI','main','push','failure')`, e.ws, e.owner, e.name); err != nil {
		t.Fatal(err)
	}
	obs := func(run int64, choice, inputs string) {
		subj := subjectID(Run{RepoOwner: e.owner, RepoName: e.name, RunID: run, RunAttempt: 1})
		if _, err := e.pool.Exec(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, choice_key, inputs)
			VALUES ($1,'ci_event',$2,'flake',$3,$4::jsonb)`, e.ws, subj, choice, inputs); err != nil {
			t.Fatal(err)
		}
	}
	obs(301, "P2", `{"route":"triage"}`) // routed, no action: Shadow would have triaged
	obs(302, "P1", `{}`)                 // routed without a route
	obs(303, "P0", `{"route":"skip"}`)   // P0 is a skip: nothing would be done
	obs(304, "P1", `{}`)                 // acted: the action row says what happened
	if _, err := e.pool.Exec(ctx, `INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind, state, protocol, reason)
		VALUES ($1,$2,$3,304,'rerun','done','P1','')`, e.ws, e.owner, e.name); err != nil {
		t.Fatal(err)
	}
	got, err := ListEvents(ctx, e.pool, e.ws, e.owner, e.name, 50)
	if err != nil || len(got) != 4 {
		t.Fatalf("events = %+v, %v", got, err)
	}
	want := map[int64]string{301: "P2 triage", 302: "P1", 303: "", 304: ""}
	for _, ev := range got {
		if ev.WouldDo != want[ev.RunID] {
			t.Errorf("run %d would_do = %q, want %q", ev.RunID, ev.WouldDo, want[ev.RunID])
		}
	}
}
