package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sqlSpy records the statement a Queries sends, so a test explains the
// production query rather than a copy of it.
type sqlSpy struct {
	DBTX
	sql  string
	args []interface{}
}

func (s *sqlSpy) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	s.sql, s.args = sql, args
	return s.DBTX.Query(ctx, sql, args...)
}

func (s *sqlSpy) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	s.sql, s.args = sql, args
	return s.DBTX.QueryRow(ctx, sql, args...)
}

func (s *sqlSpy) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	s.sql, s.args = sql, args
	return s.DBTX.Exec(ctx, sql, args...)
}

// planCost is what a statement cost the executor, read from EXPLAIN (ANALYZE,
// BUFFERS, TIMING OFF, FORMAT JSON). Neither number depends on host speed.
type planCost struct {
	// RowsExamined sums, over every plan node, (Actual Rows + Rows Removed by
	// Filter / Index Recheck / Join Filter) x Actual Loops: every row the node
	// read, kept or not. Output rows alone hide a scan that filters them away.
	RowsExamined float64
	// Buffers is Shared Hit Blocks + Shared Read Blocks of the top node.
	Buffers int64
	// SeqScans lists the relations read by a Seq Scan node.
	SeqScans []string
}

func explainCost(ctx context.Context, tx DBTX, sql string, args ...interface{}) (planCost, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return planCost{}, err
	}
	var out []struct{ Plan map[string]any }
	if err := json.Unmarshal(raw, &out); err != nil {
		return planCost{}, err
	}
	if len(out) != 1 {
		return planCost{}, fmt.Errorf("explain returned %d plans", len(out))
	}
	var cost planCost
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		num := func(key string) float64 { v, _ := node[key].(float64); return v }
		loops := num("Actual Loops")
		cost.RowsExamined += (num("Actual Rows") + num("Rows Removed by Filter") +
			num("Rows Removed by Index Recheck") + num("Rows Removed by Join Filter")) * loops
		if node["Node Type"] == "Seq Scan" {
			rel, _ := node["Relation Name"].(string)
			cost.SeqScans = append(cost.SeqScans, rel)
		}
		subs, _ := node["Plans"].([]any)
		for _, sub := range subs {
			walk(sub.(map[string]any))
		}
	}
	walk(out[0].Plan)
	hit, _ := out[0].Plan["Shared Hit Blocks"].(float64)
	read, _ := out[0].Plan["Shared Read Blocks"].(float64)
	cost.Buffers = int64(hit + read)
	return cost, nil
}
