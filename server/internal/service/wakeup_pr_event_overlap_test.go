package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type overlapStarter struct {
	inner  TxStarter
	inject func()
}

func (starter *overlapStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &overlapTx{Tx: transaction, inject: starter.inject}, nil
}

type overlapTx struct {
	pgx.Tx
	inject func()
}

func (transaction *overlapTx) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	if strings.Contains(query, "-- name: LockWakeupIssue") && transaction.inject != nil {
		inject := transaction.inject
		transaction.inject = nil
		inject()
	}
	return transaction.Tx.QueryRow(ctx, query, arguments...)
}

func TestFallbackSelectedBeforeLegacyCommitStillWakesOnce(t *testing.T) {
	for _, kind := range []string{"checks_failed", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			env := newCoalesceEnv(t)
			event := env.captured(t, kind)
			original := env.s.Tasks.TxStarter
			legacy := &IssueWakeupService{Tasks: &TaskService{Queries: env.s.Tasks.Queries, TxStarter: original, Bus: env.s.Tasks.Bus, Hub: env.s.Tasks.Hub}}
			injected := false
			env.s.Tasks.TxStarter = &overlapStarter{inner: original, inject: func() {
				injected = true
				input := PullRequestWakeupInput{Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 300, URL: "https://github.com/acme/widget/pull/300", HeadSHA: "h1", Conclusion: "FAILURE"}
				if kind == "conflict" {
					input.Rule = SystemRulePRAttention
					input.Reason = PRAttentionMergeability
					input.MergeState = "DIRTY"
					input.BaseBranch = "main"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := legacy.TriggerPullRequestWakeup(ctx, env.issue, input); err != nil {
					t.Fatal(err)
				}
			}}
			defer func() { env.s.Tasks.TxStarter = original }()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := env.s.queueNextTurnPREvents(ctx, []pgtype.UUID{parseTestUUID(t, env.f.WorkspaceID)}); err != nil {
				t.Fatal(err)
			}
			env.s.Tasks.TxStarter = original
			if !injected {
				t.Fatal("legacy callback was not injected between selection and issue lock")
			}
			env.finishTasks(t)
			env.sweep(t)
			if got := env.runs(t); got != 1 {
				t.Fatalf("legacy committed between selection and issue lock: %s started %d runs, want 1; event=%s", kind, got, env.status(t, event))
			}
		})
	}
}
