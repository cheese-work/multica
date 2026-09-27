package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

type fakeGovernanceRetentionLister struct {
	workspaces []pgtype.UUID
	err        error
}

func (f fakeGovernanceRetentionLister) ListGovernanceCaseRetentionWorkspaces(context.Context) ([]pgtype.UUID, error) {
	return f.workspaces, f.err
}

type fakeGovernanceRetentionSweeper struct {
	calls  []pgtype.UUID
	failOn string
}

func (f *fakeGovernanceRetentionSweeper) SweepWorkspace(_ context.Context, workspaceID pgtype.UUID) (int64, error) {
	f.calls = append(f.calls, workspaceID)
	if util.UUIDToString(workspaceID) == f.failOn {
		return 0, errors.New("synthetic sweep failure")
	}
	return 3, nil
}

func governanceRetentionUUID(value byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{15: value}, Valid: true}
}

func TestGovernanceCaseEvidenceRetentionSweepsWorkspaceTransactions(t *testing.T) {
	first := governanceRetentionUUID(1)
	second := governanceRetentionUUID(2)
	sweeper := &fakeGovernanceRetentionSweeper{}
	handler := makeGovernanceCaseEvidenceRetentionHandler(
		fakeGovernanceRetentionLister{workspaces: []pgtype.UUID{first, second}}, sweeper,
	)
	result, err := handler(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatalf("retention handler failed: %v", err)
	}
	if result.RowsAffected != 6 {
		t.Fatalf("RowsAffected = %d, want 6", result.RowsAffected)
	}
	if got := result.Result["workspaces_swept"]; got != 2 {
		t.Fatalf("workspaces_swept = %v, want 2", got)
	}
	if len(sweeper.calls) != 2 || sweeper.calls[0] != first || sweeper.calls[1] != second {
		t.Fatalf("swept workspaces = %v, want both in order", sweeper.calls)
	}
}

func TestGovernanceCaseEvidenceRetentionFailsClosed(t *testing.T) {
	listFailure := errors.New("synthetic workspace listing failure")
	if _, err := makeGovernanceCaseEvidenceRetentionHandler(
		fakeGovernanceRetentionLister{err: listFailure}, &fakeGovernanceRetentionSweeper{},
	)(context.Background(), HandlerInput{}); !errors.Is(err, listFailure) {
		t.Fatalf("list error = %v, want wrapped list failure", err)
	}

	first := governanceRetentionUUID(1)
	second := governanceRetentionUUID(2)
	sweeper := &fakeGovernanceRetentionSweeper{failOn: util.UUIDToString(second)}
	_, err := makeGovernanceCaseEvidenceRetentionHandler(
		fakeGovernanceRetentionLister{workspaces: []pgtype.UUID{first, second, governanceRetentionUUID(3)}}, sweeper,
	)(context.Background(), HandlerInput{})
	if err == nil || !strings.Contains(err.Error(), "synthetic sweep failure") {
		t.Fatalf("sweep error = %v, want wrapped failure", err)
	}
	if len(sweeper.calls) != 2 {
		t.Fatalf("sweeper calls = %d, want stop at failed workspace", len(sweeper.calls))
	}
}

func TestGovernanceCaseEvidenceRetentionHeartbeat(t *testing.T) {
	workspaces := make([]pgtype.UUID, 201)
	for i := range workspaces {
		workspaces[i] = governanceRetentionUUID(byte(i))
	}
	sweeper := &fakeGovernanceRetentionSweeper{}
	var heartbeats int
	_, err := makeGovernanceCaseEvidenceRetentionHandler(
		fakeGovernanceRetentionLister{workspaces: workspaces}, sweeper,
	)(context.Background(), HandlerInput{Heartbeat: func(context.Context) error {
		heartbeats++
		return nil
	}})
	if err != nil {
		t.Fatalf("retention handler failed: %v", err)
	}
	if heartbeats != 1 {
		t.Fatalf("heartbeats = %d, want 1", heartbeats)
	}
}

func TestGovernanceCaseEvidenceRetentionJobConfiguration(t *testing.T) {
	job := GovernanceCaseEvidenceRetentionJob(nil, nil)
	if job.Name != JobNameGovernanceCaseEvidenceRetention || job.Cadence <= 0 || job.RunTimeout <= 0 {
		t.Fatalf("unexpected retention job configuration: %+v", job)
	}
	if job.Handler == nil {
		t.Fatal("retention job has no handler")
	}
}
