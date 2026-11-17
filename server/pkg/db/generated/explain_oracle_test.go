package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-6850 performance oracle. EXPLAIN (ANALYZE, BUFFERS, TIMING OFF) of the
// production statement, then two numbers that do not depend on the host's speed:
//
//   - rowsExamined: for every plan node, (Actual Rows + Rows Removed by Filter +
//     Rows Removed by Index Recheck + Rows Removed by Join Filter) x Actual Loops.
//     Output rows alone (what a row-count check sees) hide a scan that reads a
//     million rows and keeps ten.
//   - buffers: Shared Hit Blocks + Shared Read Blocks of the top node.
type explainWork struct {
	rowsExamined int64
	buffers      int64
}

type explainNode struct {
	ActualRows       float64       `json:"Actual Rows"`
	ActualLoops      float64       `json:"Actual Loops"`
	RemovedByFilter  float64       `json:"Rows Removed by Filter"`
	RemovedByRecheck float64       `json:"Rows Removed by Index Recheck"`
	RemovedByJoin    float64       `json:"Rows Removed by Join Filter"`
	SharedHitBlocks  int64         `json:"Shared Hit Blocks"`
	SharedReadBlocks int64         `json:"Shared Read Blocks"`
	Plans            []explainNode `json:"Plans"`
}

func (n explainNode) rows() float64 {
	// A node that never ran (Actual Loops 0) examined nothing.
	total := (n.ActualRows + n.RemovedByFilter + n.RemovedByRecheck + n.RemovedByJoin) * n.ActualLoops
	for _, child := range n.Plans {
		total += child.rows()
	}
	return total
}

func readExplainWork(raw []byte) (explainWork, error) {
	var plans []struct {
		Plan explainNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		return explainWork{}, fmt.Errorf("read plan: %w: %s", err, raw)
	}
	top := plans[0].Plan
	return explainWork{rowsExamined: int64(top.rows()), buffers: top.SharedHitBlocks + top.SharedReadBlocks}, nil
}

// explainOracle runs EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) of a
// statement inside tx and returns its work. A write statement is explained too,
// so the caller's transaction must roll back.
func explainOracle(ctx context.Context, tx pgx.Tx, sql string, args ...any) (explainWork, []byte, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return explainWork{}, nil, err
	}
	work, err := readExplainWork(raw)
	return work, raw, err
}

// oracleTx opens a transaction on the test database or skips the test.
func oracleTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Skip(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) }) // a seed never leaves its transaction
	return ctx, tx
}

func TestExplainOracleCountsEveryNode(t *testing.T) {
	raw := []byte(`[{"Plan":{"Actual Rows":1,"Actual Loops":1,"Shared Hit Blocks":7,"Shared Read Blocks":3,"Plans":[
		{"Actual Rows":10,"Actual Loops":2,"Rows Removed by Filter":5,"Rows Removed by Index Recheck":1,"Rows Removed by Join Filter":4},
		{"Actual Rows":0,"Actual Loops":0,"Rows Removed by Filter":9,"Plans":[{"Actual Rows":3,"Actual Loops":4}]}]}}]`)
	got, err := readExplainWork(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 1*1 + (10+5+1+4)*2 + (0+9)*0 (a node that never ran examined nothing) + 3*4
	if want := (explainWork{rowsExamined: 1 + 40 + 0 + 12, buffers: 10}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
