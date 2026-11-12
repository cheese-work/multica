package service

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Static inventory deliberately excludes SQL assembled at runtime. ClaimAgentTask
// supplies the independent dispatch backstop for paths outside this scan.
func unguardedProviderHoldPromotions(root string) ([]string, error) {
	var violations []string
	comments := regexp.MustCompile(`(?m)--[^\n]*|(?s:/\*.*?\*/)`)
	update := regexp.MustCompile(`(?is)\bUPDATE\s+(?:public\.)?agent_task_queue(?:\s+\w+)?\s+SET\b(.*?)\bWHERE\b`)
	queued := regexp.MustCompile(`(?is)\bstatus\s*=\s*'queued'`)
	deferred := regexp.MustCompile(`(?is)\bstatus\s*=\s*'deferred'`)
	guard := regexp.MustCompile(`(?is)NOT\s*\(\s*(?:COALESCE\(\s*)?(?:\w+\.)?context(?:\s*,\s*'{}'::jsonb\s*\))?\s*\?\s*'provider_hold'\s*\)`)
	scan := func(path, sql string) {
		// This one named promoter is chat-only; it cannot reach issue holds.
		chatOnly := strings.Contains(sql, "-- name: PromoteChannelChatTasksIfMediaReady ")
		for _, statement := range strings.Split(comments.ReplaceAllString(sql, ""), ";") {
			assignment := update.FindStringSubmatch(statement)
			if len(assignment) == 0 || !queued.MatchString(assignment[1]) || !deferred.MatchString(statement) || guard.MatchString(statement) {
				continue
			}
			if chatOnly && regexp.MustCompile(`(?i)\bissue_id\s+IS\s+NULL`).MatchString(statement) {
				continue
			}
			violations = append(violations, path)
		}
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		isGo := strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go")
		isQuery := strings.HasPrefix(rel, "pkg/db/queries/") && strings.HasSuffix(rel, ".sql")
		isMigration := strings.HasPrefix(rel, "migrations/") && strings.HasSuffix(rel, ".up.sql")
		if !isGo && !isQuery && !isMigration {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if isGo {
			file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					if sql, err := strconv.Unquote(literal.Value); err == nil {
						scan(rel, sql)
					}
				}
				return true
			})
		} else if isQuery {
			// Bound each allowlist entry to its named statement, not the file.
			for _, query := range strings.Split(string(data), "-- name:")[1:] {
				scan(rel+"#"+strings.Fields(query)[0], "-- name:"+query)
			}
		} else {
			scan(rel, string(data))
		}
		return nil
	})
	sort.Strings(violations)
	return violations, err
}

func TestProviderHoldPromotionInventory(t *testing.T) {
	violations, err := unguardedProviderHoldPromotions(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("deferred promotions lack provider_hold exclusion: %v", violations)
	}
}

func TestProviderHoldInventoryRejectsNewPaths(t *testing.T) {
	for _, path := range []string{"pkg/db/queries/fake.sql", "internal/fake.go", "migrations/99999_fake.up.sql"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); err != nil {
				t.Fatal(err)
			}
			sql := "-- name: FakePromotion :exec\nUPDATE agent_task_queue SET status = 'queued' WHERE status = 'deferred'"
			if strings.HasSuffix(path, ".go") {
				sql = "package fake\nconst query = " + strconv.Quote(sql)
			}
			if err := os.WriteFile(filepath.Join(root, path), []byte(sql), 0600); err != nil {
				t.Fatal(err)
			}
			violations, err := unguardedProviderHoldPromotions(root)
			if err != nil || len(violations) != 1 {
				t.Fatalf("new unguarded promoter: violations=%v err=%v; want one", violations, err)
			}
		})
	}
}

func TestProviderHoldBlocksPromotionAndClaims(t *testing.T) {
	for _, path := range []string{"runtime", "runtimes", "media", "claim", "candidate", "candidates"} {
		t.Run(path, func(t *testing.T) {
			r := newBreakerRig(t)
			ctx := context.Background()
			q := db.New(r.pool)
			status := "deferred"
			if path == "claim" || strings.HasPrefix(path, "candidate") {
				status = "queued"
			}
			id := r.task(t, status, "", 0)
			if _, err := r.pool.Exec(ctx, `UPDATE agent_task_queue SET fire_at=now()-interval '1 minute', context='{"keep":true,"provider_hold":{"provider":"openai","since":"2026-11-12T00:00:00Z","max_hold_until":"2099-01-01T00:00:00Z"}}' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			attempt := func() bool {
				var promoted bool
				switch path {
				case "runtime":
					rows, err := q.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{RuntimeID: r.runtimeID, RuntimeStaleSecs: 60})
					if err != nil {
						t.Fatal(err)
					}
					promoted = len(rows) != 0
				case "runtimes":
					rows, err := q.PromoteDueDeferredTasksForRuntimes(ctx, db.PromoteDueDeferredTasksForRuntimesParams{RuntimeIds: []pgtype.UUID{r.runtimeID}, RuntimeStaleSecs: 60})
					if err != nil {
						t.Fatal(err)
					}
					promoted = len(rows) != 0
				case "media":
					row, err := q.PromoteDeferredChannelIssueTask(ctx, id)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal(err)
					}
					promoted = row.ID.Valid
				case "claim":
					row, err := q.ClaimAgentTask(ctx, db.ClaimAgentTaskParams{AgentID: r.agentID, RuntimeID: r.runtimeID, RuntimeStaleSecs: 60, PrepareLeaseSecs: 60})
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						t.Fatal(err)
					}
					promoted = row.ID.Valid
				case "candidate":
					rows, err := q.ListQueuedClaimCandidatesByRuntime(ctx, r.runtimeID)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						promoted = promoted || row.ID == id
					}
				case "candidates":
					rows, err := q.ListQueuedClaimCandidatesByRuntimes(ctx, []pgtype.UUID{r.runtimeID})
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						promoted = promoted || row.ID == id
					}
				}
				return promoted
			}
			if attempt() {
				t.Fatal("provider-held task became promotable or claimable")
			}
			if status == "deferred" {
				next, err := q.NextDeferredTaskFireAtForRuntimes(ctx, db.NextDeferredTaskFireAtForRuntimesParams{RuntimeIds: []pgtype.UUID{r.runtimeID}, RuntimeStaleSecs: 60})
				if err != nil || next.Valid {
					t.Fatalf("held task advertised: %v, %v", next, err)
				}
			}
			r.breaker(t, "closed", time.Hour, 300, pgtype.UUID{})
			breaker, err := q.GetProviderBreaker(ctx, db.GetProviderBreakerParams{WorkspaceID: r.workspaceID, Provider: "openai"})
			if err != nil {
				t.Fatal(err)
			}
			released, err := q.ReleaseProviderHeldTask(ctx, db.ReleaseProviderHeldTaskParams{WorkspaceID: r.workspaceID, Provider: "openai", ExpectedVersion: breaker.Version, TaskID: id})
			if err != nil {
				t.Fatal(err)
			}
			var metadata map[string]any
			if err := json.Unmarshal(released.Context, &metadata); err != nil {
				t.Fatal(err)
			}
			if _, exists := metadata["provider_hold"]; exists || metadata["keep"] != true || released.Status != status {
				t.Fatalf("release changed unrelated state: %s %s", released.Status, released.Context)
			}
			if status == "deferred" {
				next, err := q.NextDeferredTaskFireAtForRuntimes(ctx, db.NextDeferredTaskFireAtForRuntimesParams{RuntimeIds: []pgtype.UUID{r.runtimeID}, RuntimeStaleSecs: 60})
				if err != nil || !next.Valid {
					t.Fatalf("released task not advertised: %v %v", next, err)
				}
			}
			if !attempt() {
				t.Fatal("released task still cannot promote or claim")
			}
		})
	}
}

func TestProviderHoldAdmission(t *testing.T) {
	for _, state := range []string{"closed", "open", "half_open", "draining"} {
		t.Run(state, func(t *testing.T) {
			r := newBreakerRig(t)
			r.breaker(t, state, time.Hour, 300, pgtype.UUID{})
			q := db.New(r.pool)
			issue, err := q.GetIssue(context.Background(), r.issue)
			if err != nil {
				t.Fatal(err)
			}
			svc := NewTaskService(q, r.pool, nil, events.New())
			task, err := svc.EnqueueTaskForIssue(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if state == "closed" {
				if task.Status != "queued" || strings.Contains(string(task.Context), "provider_hold") {
					t.Fatalf("closed admission: %s %s", task.Status, task.Context)
				}
			} else if task.Status != "deferred" || !strings.Contains(string(task.Context), "provider_hold") {
				t.Fatalf("%s admission = %s %s; want durable deferred provider_hold", state, task.Status, task.Context)
			}
		})
	}
}
