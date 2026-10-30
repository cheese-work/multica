package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/jev"
)

// noJevFixture is a squad whose workspace has Jev routing enabled and a mock
// Jev server that would route every request to the worker.
type noJevFixture struct {
	squadID, leaderID, workerID pgtype.UUID
	jevCalls                    *atomic.Int32
}

func newNoJevFixture(t *testing.T) noJevFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	ctx := context.Background()
	wsUUID := util.MustParseUUID(testWorkspaceID)
	rtUUID := util.MustParseUUID(testRuntimeID)
	userUUID := util.MustParseUUID(testUserID)

	f := noJevFixture{squadID: dbid.NewV7(), leaderID: dbid.NewV7(), workerID: dbid.NewV7(), jevCalls: &atomic.Int32{}}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $3, $4, $5, 'nojev-leader', 'idle', 'cloud'),
		       ($2, $3, $4, $5, 'nojev-worker', 'idle', 'cloud')
	`, f.leaderID, f.workerID, wsUUID, rtUUID, userUUID); err != nil {
		t.Fatalf("insert agents: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, f.leaderID, f.workerID) })

	if _, err := testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, 'NoJev Squad', '', 'Tier S: nojev-worker', $3, $4)
	`, f.squadID, wsUUID, f.leaderID, userUUID); err != nil {
		t.Fatalf("insert squad: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, f.squadID) })
	if _, err := testPool.Exec(ctx, `
		INSERT INTO squad_member (id, squad_id, member_type, member_id, role)
		VALUES ($1, $2, 'agent', $3, 'worker')
	`, dbid.NewV7(), f.squadID, f.workerID); err != nil {
		t.Fatalf("insert squad member: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.jevCalls.Add(1)
		worker := util.UUIDToString(f.workerID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{"route_squad_member": {
				Type: jev.TypeChoice, Choice: worker, Confidence: 0.95,
				Probabilities: map[string]float64{worker: 0.95, "escalate_to_leader": 0.05},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	minConf := 0.90
	settingsJSON, _ := json.Marshal(governance.WorkspaceSettings{
		JevSquadRoutingEnabled: true, JevAPIURL: srv.URL, JevMinConfidence: &minConf,
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON); err != nil {
		t.Fatalf("insert governance config: %v", err)
	}
	client, _ := jev.NewClient(jev.Options{APIKey: "test-jev-key", BaseURL: srv.URL})
	orig := testHandler.Jev
	testHandler.Jev = client
	t.Cleanup(func() { testHandler.Jev = orig })
	return f
}

// assertLeaderOnly checks the dispatch behaved as with jev_enabled off: Jev
// never called, only the leader queued, no jev_route comment.
func (f noJevFixture) assertLeaderOnly(t *testing.T, issueID pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	if n := f.jevCalls.Load(); n != 0 {
		t.Errorf("Jev called %d times, want 0", n)
	}
	rows, err := testPool.Query(ctx, `SELECT agent_id FROM agent_task_queue WHERE issue_id = $1`, issueID)
	if err != nil {
		t.Fatalf("query tasks: %v", err)
	}
	defer rows.Close()
	var agents []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		agents = append(agents, util.UUIDToString(id))
	}
	if len(agents) != 1 || agents[0] != util.UUIDToString(f.leaderID) {
		t.Errorf("queued agents = %v, want only leader %s", agents, util.UUIDToString(f.leaderID))
	}
	var routeComments int
	_ = testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'jev_route'`, issueID).Scan(&routeComments)
	if routeComments != 0 {
		t.Errorf("jev_route comments = %d, want 0", routeComments)
	}
}

func TestSquadAssign_DoesNotRouteThroughJev(t *testing.T) {
	f := newNoJevFixture(t)
	ctx := context.Background()
	res, err := testHandler.IssueService.Create(ctx, service.IssueCreateParams{
		WorkspaceID:  util.MustParseUUID(testWorkspaceID),
		Title:        "no-jev assign",
		Status:       "todo",
		Priority:     "high",
		AssigneeType: pgtype.Text{String: "squad", Valid: true},
		AssigneeID:   f.squadID,
		CreatorType:  "member",
		CreatorID:    util.MustParseUUID(testUserID),
	}, service.IssueCreateOpts{})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, res.Issue.ID)
	f.assertLeaderOnly(t, res.Issue.ID)
}

func TestSquadMentionComment_DoesNotRouteThroughJev(t *testing.T) {
	f := newNoJevFixture(t)
	ctx := context.Background()
	wsUUID := util.MustParseUUID(testWorkspaceID)
	issueID := dbid.NewV7()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'no-jev comment', '', 'todo', 'high', 'squad', $3, 'member', $4, 1, 4010)
	`, issueID, wsUUID, f.squadID, testUserID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	defer testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)

	content := fmt.Sprintf("@[NoJev Squad](mention://squad/%s) please handle this", util.UUIDToString(f.squadID))
	w := httptest.NewRecorder()
	r := newRequest("POST", "/api/issues/"+util.UUIDToString(issueID)+"/comments", map[string]any{"content": content})
	r = withURLParam(r, "id", util.UUIDToString(issueID))
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment = %d: %s", w.Code, w.Body.String())
	}
	f.assertLeaderOnly(t, issueID)
}
