package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func instructionPairFixture(t *testing.T) (workspaceID, squadID string) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := fmt.Sprintf("instruction-pair-%d", time.Now().UnixNano())
	workspaceID = dbfx.Workspace(t, "Instruction Pair", name, testutil.Cols{"context": "old context"})
	dbfx.Member(t, workspaceID, testUserID, "owner")
	runtimeID := dbfx.Runtime(t, name+"-runtime", testutil.Cols{"workspace_id": workspaceID, "owner_id": testUserID})
	leaderID := dbfx.Agent(t, name+"-leader", runtimeID, testutil.Cols{"workspace_id": workspaceID, "owner_id": testUserID})
	squadID = dbfx.Squad(t, name, leaderID, testutil.Cols{
		"workspace_id": workspaceID,
		"instructions": "old instructions",
	})
	return workspaceID, squadID
}

func instructionPairRequest(workspaceID, squadID, contextText, instructionsText, contextDigest, instructionsDigest string) *http.Request {
	request := newRequest(http.MethodPut, "/api/workspaces/"+workspaceID+"/instruction-pair", map[string]any{
		"context":                             contextText,
		"squad_id":                            squadID,
		"instructions":                        instructionsText,
		"expected_context_before_digest":      contextDigest,
		"expected_instructions_before_digest": instructionsDigest,
	})
	request.Header.Set("X-User-ID", testUserID)
	return withURLParam(request, "id", workspaceID)
}

func instructionPairStored(t *testing.T, workspaceID, squadID string) (string, string, time.Time, time.Time) {
	t.Helper()
	var contextText, instructionsText string
	var contextUpdatedAt, instructionsUpdatedAt time.Time
	dbfx.QueryRow(t, `SELECT context, updated_at FROM workspace WHERE id = $1`, workspaceID).Scan(&contextText, &contextUpdatedAt)
	dbfx.QueryRow(t, `SELECT instructions, updated_at FROM squad WHERE id = $1`, squadID).Scan(&instructionsText, &instructionsUpdatedAt)
	return contextText, instructionsText, contextUpdatedAt, instructionsUpdatedAt
}

func TestApplyInstructionPairUpdatesBothFieldsInOneOperation(t *testing.T) {
	workspaceID, squadID := instructionPairFixture(t)
	contextText := "candidate context\r\nwith exact bytes 漢字"
	instructionsText := "candidate instructions\nwith exact bytes"
	request := instructionPairRequest(workspaceID, squadID, contextText, instructionsText,
		instructionPairDigest("old context"), instructionPairDigest("old instructions"))
	response := httptest.NewRecorder()
	testHandler.ApplyInstructionPair(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	storedContext, storedInstructions, _, _ := instructionPairStored(t, workspaceID, squadID)
	if storedContext != contextText || storedInstructions != instructionsText {
		t.Fatalf("stored pair = (%q, %q), want exact candidates", storedContext, storedInstructions)
	}
	var result applyInstructionPairResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.ContextAfterDigest != instructionPairDigest(contextText) || result.InstructionsAfterDigest != instructionPairDigest(instructionsText) {
		t.Fatalf("after digests = (%s, %s), candidates do not match", result.ContextAfterDigest, result.InstructionsAfterDigest)
	}
}

func TestApplyInstructionPairRejectsEitherStaleDigestWithoutUpdatingEitherField(t *testing.T) {
	for _, staleField := range []string{"context", "instructions"} {
		t.Run(staleField, func(t *testing.T) {
			workspaceID, squadID := instructionPairFixture(t)
			_, _, beforeContextUpdatedAt, beforeInstructionsUpdatedAt := instructionPairStored(t, workspaceID, squadID)
			contextDigest := instructionPairDigest("old context")
			instructionsDigest := instructionPairDigest("old instructions")
			if staleField == "context" {
				contextDigest = instructionPairDigest("stale context")
			} else {
				instructionsDigest = instructionPairDigest("stale instructions")
			}
			response := httptest.NewRecorder()
			testHandler.ApplyInstructionPair(response, instructionPairRequest(
				workspaceID, squadID, "candidate context", "candidate instructions", contextDigest, instructionsDigest,
			))
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
			}
			contextText, instructionsText, contextUpdatedAt, instructionsUpdatedAt := instructionPairStored(t, workspaceID, squadID)
			if contextText != "old context" || instructionsText != "old instructions" {
				t.Fatalf("stale pair changed fields: context=%q instructions=%q", contextText, instructionsText)
			}
			if !contextUpdatedAt.Equal(beforeContextUpdatedAt) || !instructionsUpdatedAt.Equal(beforeInstructionsUpdatedAt) {
				t.Fatalf("stale pair performed an update: before=(%v,%v) after=(%v,%v)", beforeContextUpdatedAt, beforeInstructionsUpdatedAt, contextUpdatedAt, instructionsUpdatedAt)
			}
		})
	}
}

type instructionPairRaceTxStarter struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	updates atomic.Int32
}

func (s *instructionPairRaceTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &instructionPairRaceTx{Tx: tx, starter: s}, nil
}

type instructionPairRaceTx struct {
	pgx.Tx
	starter *instructionPairRaceTxStarter
}

func (tx *instructionPairRaceTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if query == instructionPairLockSquadQuery {
		tx.starter.once.Do(func() { close(tx.starter.reached) })
		<-tx.starter.release
	}
	return tx.Tx.QueryRow(ctx, query, args...)
}

func (tx *instructionPairRaceTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(query, "UPDATE workspace SET context") || strings.HasPrefix(query, "UPDATE squad SET instructions") {
		tx.starter.updates.Add(1)
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestApplyInstructionPairChecksDigestsAfterExecutionTimeRace(t *testing.T) {
	workspaceID, squadID := instructionPairFixture(t)
	_, _, beforeContextUpdatedAt, _ := instructionPairStored(t, workspaceID, squadID)
	starter := &instructionPairRaceTxStarter{reached: make(chan struct{}), release: make(chan struct{})}
	handler := *testHandler
	handler.TxStarter = starter
	request := instructionPairRequest(workspaceID, squadID, "candidate context", "candidate instructions",
		instructionPairDigest("old context"), instructionPairDigest("old instructions"))
	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ApplyInstructionPair(response, request)
		responseCh <- response
	}()

	var response *httptest.ResponseRecorder
	select {
	case <-starter.reached:
	case <-time.After(5 * time.Second):
		close(starter.release)
		<-responseCh
		t.Fatal("paired transaction did not reach the locked squad read")
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE squad SET instructions = $1, updated_at = '2026-01-01T00:00:00Z' WHERE id = $2`, "raced instructions", squadID); err != nil {
		close(starter.release)
		<-responseCh
		t.Fatalf("write concurrent squad change: %v", err)
	}
	close(starter.release)
	response = <-responseCh
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want execution-time 409: %s", response.Code, response.Body.String())
	}
	if updates := starter.updates.Load(); updates != 0 {
		t.Fatalf("paired transaction updates = %d, want zero after a digest race", updates)
	}
	contextText, instructionsText, contextUpdatedAt, instructionsUpdatedAt := instructionPairStored(t, workspaceID, squadID)
	if contextText != "old context" || instructionsText != "raced instructions" {
		t.Fatalf("paired attempt overwrote state after the race: context=%q instructions=%q", contextText, instructionsText)
	}
	if !contextUpdatedAt.Equal(beforeContextUpdatedAt) || !instructionsUpdatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("paired attempt wrote despite stale digest: timestamps=(%v,%v)", contextUpdatedAt, instructionsUpdatedAt)
	}
}

type instructionPairFailSquadUpdateStarter struct{}

func (instructionPairFailSquadUpdateStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := testPool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return instructionPairFailSquadUpdateTx{Tx: tx}, nil
}

type instructionPairFailSquadUpdateTx struct {
	pgx.Tx
}

func (tx instructionPairFailSquadUpdateTx) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(query, "UPDATE squad SET instructions") {
		return pgconn.CommandTag{}, errors.New("injected squad update failure")
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestApplyInstructionPairRollsBackWorkspaceWhenSquadUpdateFails(t *testing.T) {
	workspaceID, squadID := instructionPairFixture(t)
	_, _, beforeContextUpdatedAt, beforeInstructionsUpdatedAt := instructionPairStored(t, workspaceID, squadID)
	handler := *testHandler
	handler.TxStarter = instructionPairFailSquadUpdateStarter{}
	response := httptest.NewRecorder()
	handler.ApplyInstructionPair(response, instructionPairRequest(
		workspaceID, squadID, "candidate context", "candidate instructions",
		instructionPairDigest("old context"), instructionPairDigest("old instructions"),
	))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
	}
	contextText, instructionsText, contextUpdatedAt, instructionsUpdatedAt := instructionPairStored(t, workspaceID, squadID)
	if contextText != "old context" || instructionsText != "old instructions" {
		t.Fatalf("failed transaction changed fields: context=%q instructions=%q", contextText, instructionsText)
	}
	if !contextUpdatedAt.Equal(beforeContextUpdatedAt) || !instructionsUpdatedAt.Equal(beforeInstructionsUpdatedAt) {
		t.Fatalf("failed transaction updated timestamps: before=(%v,%v) after=(%v,%v)", beforeContextUpdatedAt, beforeInstructionsUpdatedAt, contextUpdatedAt, instructionsUpdatedAt)
	}
}

func TestApplyInstructionPairRejectsMachineActor(t *testing.T) {
	workspaceID, squadID := instructionPairFixture(t)
	request := instructionPairRequest(workspaceID, squadID, "candidate context", "candidate instructions",
		instructionPairDigest("old context"), instructionPairDigest("old instructions"))
	request.Header.Set("X-Actor-Source", "task_token")
	response := httptest.NewRecorder()
	testHandler.ApplyInstructionPair(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	contextText, instructionsText, _, _ := instructionPairStored(t, workspaceID, squadID)
	if contextText != "old context" || instructionsText != "old instructions" {
		t.Fatalf("machine actor changed pair: context=%q instructions=%q", contextText, instructionsText)
	}
}
