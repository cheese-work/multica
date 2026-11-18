package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// explainWork runs EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) on sql
// with args and returns the work the executor did, which does not depend on how
// busy the machine is (CHE-4266: a wall-clock bound failed on a loaded CI
// database).
//
//   - rowsExamined: for every plan node, (Actual Rows + Rows Removed by Filter +
//     Rows Removed by Index Recheck + Rows Removed by Join Filter) x Actual
//     Loops, summed. A row the executor read and then rejected counts. This is
//     the number planWork misses: it counts output rows only, so a scan that
//     reads 5,000 rows to return one looks the same as an index probe.
//   - buffers: Shared Hit Blocks + Shared Read Blocks of the top node.
func explainWork(t *testing.T, sql string, args ...any) (rowsExamined, buffers float64) {
	t.Helper()
	var raw []byte
	err := testPool.QueryRow(context.Background(),
		"EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+sql, args...).Scan(&raw)
	if err != nil {
		t.Fatalf("explain: %v\nSQL: %s", err, sql)
	}
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("decode explain output: %v\n%s", err, raw)
	}
	top := doc[0].Plan
	hit, _ := top["Shared Hit Blocks"].(float64)
	read, _ := top["Shared Read Blocks"].(float64)
	return planRowsExamined(top), hit + read
}

// planRowsExamined is the per-node sum explainWork documents.
func planRowsExamined(node map[string]any) float64 {
	loops, _ := node["Actual Loops"].(float64)
	var perLoop float64
	for _, key := range []string{
		"Actual Rows",
		"Rows Removed by Filter",
		"Rows Removed by Index Recheck",
		"Rows Removed by Join Filter",
	} {
		n, _ := node[key].(float64)
		perLoop += n
	}
	work := perLoop * loops
	subplans, _ := node["Plans"].([]any)
	for _, sub := range subplans {
		work += planRowsExamined(sub.(map[string]any))
	}
	return work
}

// explainText is the human-readable EXPLAIN (ANALYZE, BUFFERS) of sql, for the
// test log and a PR body.
func explainText(t *testing.T, sql string, args ...any) string {
	t.Helper()
	rows, err := testPool.Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF) "+sql, args...)
	if err != nil {
		t.Fatalf("explain text: %v", err)
	}
	defer rows.Close()
	var out strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain line: %v", err)
		}
		out.WriteString(line + "\n")
	}
	return out.String()
}
