package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// reportRow is one (scope, predictor, version) line of GET /api/system1/report.
type reportRow struct {
	Scope            string  `json:"scope"`
	PredictorID      string  `json:"predictor_id"`
	PredictorVersion string  `json:"predictor_version"`
	Status           string  `json:"status"`
	Denominator      int64   `json:"denominator"`
	Agreed           int64   `json:"agreed"`
	Abstained        int64   `json:"abstained"`
	Unmapped         int64   `json:"unmapped"`
	Reversed         int64   `json:"reversed"`
	Agreement        float64 `json:"agreement"`
}

// CHE-4383 B5 over the real router: epochs, the case and denominator rules and the status per scope, with the CHE-3239 bar unchanged.
func TestSystem1QualificationReportRoute(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	now := time.Now()
	day := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	const (
		p1    = "ci:cheese-work/multica:P1"
		p2    = "ci:cheese-work/multica:P2"
		p3    = "ci:cheese-work/multica:P3"
		waive = "test_first_waiver"
	)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM system1_label WHERE workspace_id=$1 AND scope = ANY($2)`, testWorkspaceID, []string{p1, p2, p3, waive})
		testPool.Exec(ctx, `DELETE FROM system1_prediction WHERE workspace_id=$1 AND scope = ANY($2)`, testWorkspaceID, []string{p1, p2, p3, waive})
		testPool.Exec(ctx, `DELETE FROM system1_epoch WHERE workspace_id=$1 AND scope = ANY($2)`, testWorkspaceID, []string{p1, p2, p3, waive})
		testPool.Exec(ctx, `DELETE FROM system1_observation WHERE workspace_id=$1 AND class='b5-fixture'`, testWorkspaceID)
	})

	// observe writes one observation, its prediction (nil key = abstained) and its labels, oldest first: the first prediction of a predictor version opens its epoch at predicted_at.
	observe := func(scope, predictor, version string, created time.Time, predicted *string, labels ...string) {
		t.Helper()
		var observation string
		if err := testPool.QueryRow(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, inputs, created_at) VALUES ($1,'decision',gen_random_uuid(),'b5-fixture','{}',$2) RETURNING id::text`, testWorkspaceID, created).Scan(&observation); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO system1_prediction (workspace_id, observation_id, scope, predictor_id, predictor_version, predicted_key, predicted_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, testWorkspaceID, observation, scope, predictor, version, predicted, created); err != nil {
			t.Fatal(err)
		}
		for index, label := range labels { // later rows are newer; a label starting with "-" is superseded
			superseded := (*time.Time)(nil)
			if label[0] == '-' {
				label, superseded = label[1:], &now
			}
			if _, err := testPool.Exec(ctx, `INSERT INTO system1_label (workspace_id, observation_id, scope, kind, source, label, superseded_at, created_at) VALUES ($1,$2,$3,'burn_in','human_answer',$4,$5,$6)`,
				testWorkspaceID, observation, scope, label, superseded, created.Add(time.Duration(index+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	key := func(s string) *string { return &s }

	// P1: epoch opens 10 days ago. 18 agree (one has a stale live row and a superseded row under the latest one), 1 labelled P2, 1 labelled unknown: denominator 20, 90% → qualified.
	for i := 0; i < 16; i++ {
		observe(p1, "ci_rules", "1", day(10).Add(time.Duration(i)*time.Minute), key("P1"), "P1")
	}
	observe(p1, "ci_rules", "1", day(10).Add(20*time.Minute), key("P1"), "P2", "-P2", "P1") // duplicate labels: the latest live row is the case
	observe(p1, "ci_rules", "1", day(10).Add(21*time.Minute), key("P1"), "-P2", "P1")       // a superseded row is never the label
	observe(p1, "ci_rules", "1", day(10).Add(22*time.Minute), key("P1"), "P2")              // another protocol: a disagreement, never out of the denominator
	observe(p1, "ci_rules", "1", day(10).Add(23*time.Minute), key("P1"), "unknown")         // `unknown` likewise
	observe(p1, "ci_rules", "1", day(10).Add(24*time.Minute), nil, "P1")                    // abstention: coverage only
	observe(p1, "ci_rules", "1", day(10).Add(25*time.Minute), nil, "P2")
	observe(p1, "ci_rules", "1", day(9), key("P1")) // no label yet: not a case
	// A row created before the epoch opened is not counted, whatever it says (no backfill).
	observe(p1, "ci_rules", "1", day(11), key("P1"), "P2")

	// P2: a different protocol in the same repo, 5 agreeing cases: 100% but too few → collecting. One protocol never promotes another.
	for i := 0; i < 5; i++ {
		observe(p2, "ci_rules", "1", day(10).Add(time.Duration(i)*time.Minute), key("P2"), "P2")
	}
	// P3: 20 agreeing cases but the epoch is 3 days old → collecting.
	for i := 0; i < 20; i++ {
		observe(p3, "ci_rules", "1", day(3).Add(time.Duration(i)*time.Minute), key("P3"), "P3")
	}

	// Waiver: 20 labelled observations, 19 abstentions and 1 correct prediction → denominator 1, collecting. An `unmapped` answer is counted apart and stays out of the denominator.
	for i := 0; i < 19; i++ {
		observe(waive, "waiver_rule", "1", day(10).Add(time.Duration(i)*time.Minute), nil, "waive")
	}
	observe(waive, "waiver_rule", "1", day(10).Add(30*time.Minute), key("waive"), "waive")
	observe(waive, "waiver_rule", "1", day(10).Add(31*time.Minute), key("waive"), "unmapped")
	// precedent@1 qualifies on 20 agreeing cases; precedent@2 is a new epoch that starts empty and does not inherit them.
	for i := 0; i < 20; i++ {
		observe(waive, "precedent", "1", day(10).Add(time.Duration(i)*time.Minute), key("refuse"), "refuse")
	}
	observe(waive, "precedent", "2", day(1), key("refuse"), "refuse")
	observe(waive, "precedent", "2", day(1).Add(time.Minute), key("refuse"), "waive")

	get := func(query string) []reportRow {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, testServer.URL+"/api/system1/report"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/system1/report%s status=%d, want 200", query, resp.StatusCode)
		}
		var out struct {
			Scopes []reportRow `json:"scopes"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Scopes
	}
	byKey := func(rows []reportRow) map[string]reportRow {
		out := map[string]reportRow{}
		for _, row := range rows {
			out[row.Scope+"|"+row.PredictorID+"@"+row.PredictorVersion] = row
		}
		return out
	}

	want := map[string]reportRow{
		p1 + "|ci_rules@1":       {Status: "qualified", Denominator: 20, Agreed: 18, Abstained: 2},
		p2 + "|ci_rules@1":       {Status: "collecting", Denominator: 5, Agreed: 5},
		p3 + "|ci_rules@1":       {Status: "collecting", Denominator: 20, Agreed: 20},
		waive + "|waiver_rule@1": {Status: "collecting", Denominator: 1, Agreed: 1, Abstained: 19, Unmapped: 1},
		waive + "|precedent@1":   {Status: "qualified", Denominator: 20, Agreed: 20},
		waive + "|precedent@2":   {Status: "collecting", Denominator: 2, Agreed: 1},
	}
	got := byKey(get(""))
	for name, expected := range want {
		row, ok := got[name]
		if !ok {
			t.Errorf("report has no row %s", name)
			continue
		}
		if row.Status != expected.Status || row.Denominator != expected.Denominator || row.Agreed != expected.Agreed || row.Abstained != expected.Abstained || row.Unmapped != expected.Unmapped {
			t.Errorf("%s = %+v, want status=%s denominator=%d agreed=%d abstained=%d unmapped=%d", name, row, expected.Status, expected.Denominator, expected.Agreed, expected.Abstained, expected.Unmapped)
		}
	}
	if row := got[p1+"|ci_rules@1"]; row.Agreement != 0.9 {
		t.Errorf("P1 agreement = %v, want 0.9", row.Agreement)
	}

	// --scope narrows the report to one scope (all its predictors).
	only := get("?scope=" + p2)
	if len(only) != 1 || only[0].Scope != p2 {
		t.Fatalf("scope filter returned %+v, want only %s", only, p2)
	}
}
