package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/jev"
)

const jevShadowLogMessage = "jev squad shadow decision"

// syncBuffer is a goroutine-safe log sink: shadow evaluation may run off the
// request goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) records() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == jevShadowLogMessage {
			out = append(out, rec)
		}
	}
	return out
}

// waitForShadowRecord polls until a shadow record for issueID is logged.
func (b *syncBuffer) waitForShadowRecord(t *testing.T, issueID pgtype.UUID) map[string]any {
	t.Helper()
	want := util.UUIDToString(issueID)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range b.records() {
			if rec["issue_id"] == want {
				return rec
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %q log record for issue %s", jevShadowLogMessage, want)
	return nil
}

func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	sink := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return sink
}

type jevShadowFixture struct {
	wsUUID   pgtype.UUID
	squadID  pgtype.UUID
	leaderID pgtype.UUID
	workerID pgtype.UUID
	squad    db.Squad
	handler  atomic.Value // http.HandlerFunc answering the mock Jev API
}

func (f *jevShadowFixture) workerChoice() string { return util.UUIDToString(f.workerID) }

// newJevShadowFixture seeds a squad (leader + one worker) and workspace Jev
// settings. settings keys are raw JSON field names so the fixture does not
// depend on the Go settings struct.
func newJevShadowFixture(t *testing.T, settings map[string]any) *jevShadowFixture {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("handler test database is unavailable")
	}
	ctx := context.Background()
	wsUUID := util.MustParseUUID(testWorkspaceID)
	userUUID := util.MustParseUUID(testUserID)
	rtUUID := util.MustParseUUID(testRuntimeID)
	suffix := uuid.NewString()[:8]

	f := &jevShadowFixture{wsUUID: wsUUID, squadID: dbid.NewV7(), leaderID: dbid.NewV7(), workerID: dbid.NewV7()}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent (id, workspace_id, runtime_id, owner_id, name, status, runtime_mode)
		VALUES ($1, $2, $3, $4, $5, 'idle', 'cloud'), ($6, $2, $3, $4, $7, 'idle', 'cloud')
	`, f.leaderID, wsUUID, rtUUID, userUUID, "shadow-leader-"+suffix, f.workerID, "shadow-worker-"+suffix); err != nil {
		t.Fatalf("insert agents: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent WHERE id IN ($1, $2)`, f.leaderID, f.workerID) })

	if _, err := testPool.Exec(ctx, `
		INSERT INTO squad (id, workspace_id, name, description, instructions, leader_id, creator_id)
		VALUES ($1, $2, $3, '', $4, $5, $6)
	`, f.squadID, wsUUID, "Shadow Squad "+suffix, "Tier S: shadow-worker-"+suffix, f.leaderID, userUUID); err != nil {
		t.Fatalf("insert squad: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM squad WHERE id = $1`, f.squadID) })
	if _, err := testPool.Exec(ctx, `
		INSERT INTO squad_member (id, squad_id, member_type, member_id, role) VALUES ($1, $2, 'agent', $3, 'worker')
	`, dbid.NewV7(), f.squadID, f.workerID); err != nil {
		t.Fatalf("insert squad member: %v", err)
	}

	f.handler.Store(http.NotFoundHandler())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.handler.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(server.Close)
	settings["jev_api_url"] = server.URL
	settings["jev_min_confidence"] = 0.90
	settingsJSON, _ := json.Marshal(settings)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO governance_workspace_config (workspace_id, config_version, control_epoch, settings)
		VALUES ($1, 1, 1, $2)
		ON CONFLICT (workspace_id) DO UPDATE SET settings = EXCLUDED.settings
	`, wsUUID, settingsJSON); err != nil {
		t.Fatalf("write governance config: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM governance_workspace_config WHERE workspace_id = $1`, wsUUID) })

	client, err := jev.NewClient(jev.Options{APIKey: "test-jev-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new jev client: %v", err)
	}
	prev := testHandler.Jev
	testHandler.Jev = client
	t.Cleanup(func() { testHandler.Jev = prev })

	squad, err := testHandler.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: f.squadID, WorkspaceID: wsUUID})
	if err != nil {
		t.Fatalf("load squad: %v", err)
	}
	f.squad = squad
	return f
}

func (f *jevShadowFixture) confidentWorkerAnswer(confidence float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		choice := f.workerChoice()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jev.Response{
			Model: "jev-1.13.0",
			Answers: map[string]jev.Answer{"route_squad_member": {
				Type:          jev.TypeChoice,
				Choice:        choice,
				Confidence:    confidence,
				Probabilities: map[string]float64{choice: confidence, "escalate_to_leader": 1 - confidence},
			}},
			Usage: jev.Usage{InputTokens: 500, OutputTokens: 10},
		})
	}
}

func (f *jevShadowFixture) insertIssue(t *testing.T, status string) db.Issue {
	t.Helper()
	ctx := context.Background()
	issueID := dbid.NewV7()
	number := 5_000_000 + rand.IntN(4_000_000)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (id, workspace_id, title, description, status, priority, assignee_type, assignee_id, creator_type, creator_id, revision, number)
		VALUES ($1, $2, 'Shadow routing issue', 'Needs a Go backend specialist', $3, 'high', 'squad', $4, 'member', $5, 1, $6)
	`, issueID, f.wsUUID, status, f.squadID, testUserID, number); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })
	issue, err := testHandler.Queries.GetIssue(ctx, issueID)
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	return issue
}

// assertNoJevSideEffects fails if Jev routing dispatched, commented, assigned,
// changed status or recorded squad activity for the issue.
func (f *jevShadowFixture) assertNoJevSideEffects(t *testing.T, before db.Issue) {
	t.Helper()
	ctx := context.Background()
	after, err := testHandler.Queries.GetIssue(ctx, before.ID)
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if after.Status != before.Status || after.Revision != before.Revision ||
		after.AssigneeType != before.AssigneeType || after.AssigneeID != before.AssigneeID {
		t.Errorf("issue mutated: status %q->%q revision %d->%d assignee %v->%v",
			before.Status, after.Status, before.Revision, after.Revision, before.AssigneeID, after.AssigneeID)
	}
	var comments, workerTasks, activities int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, before.ID).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, before.ID, f.workerID).Scan(&workerTasks); err != nil {
		t.Fatalf("count worker tasks: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'squad_leader_evaluated'`, before.ID).Scan(&activities); err != nil {
		t.Fatalf("count activities: %v", err)
	}
	if comments != 0 || workerTasks != 0 || activities != 0 {
		t.Errorf("side effects: comments=%d worker_tasks=%d squad_activities=%d, want all 0", comments, workerTasks, activities)
	}
}

func TestJevShadow_RecordsDecisionWithoutAnySideEffect(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(f.confidentWorkerAnswer(0.95))
	issue := f.insertIssue(t, "backlog")
	trigger := dbid.NewV7()

	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID, trigger)
	if routed || advisory != "" {
		t.Fatalf("shadow must hand over to the leader untouched: routed=%v advisory=%q", routed, advisory)
	}

	rec := logs.waitForShadowRecord(t, issue.ID)
	for key, want := range map[string]any{
		"workspace_id":       testWorkspaceID,
		"squad_id":           util.UUIDToString(f.squadID),
		"leader_id":          util.UUIDToString(f.leaderID),
		"trigger_comment_id": util.UUIDToString(trigger),
		"winner_id":          f.workerChoice(),
		"outcome":            "decision",
		"decision_source":    "jev",
		"confidence":         0.95,
	} {
		if rec[key] != want {
			t.Errorf("record[%s] = %v, want %v", key, rec[key], want)
		}
	}
	if id, _ := rec["decision_id"].(string); !strings.HasPrefix(id, "jd_") {
		t.Errorf("record[decision_id] = %v, want jd_ prefix", rec["decision_id"])
	}
	if _, ok := rec["latency_ms"]; !ok {
		t.Error("record is missing latency_ms")
	}
	if _, ok := rec["error"]; ok {
		t.Errorf("successful decision must not carry error, got %v", rec["error"])
	}
	f.assertNoJevSideEffects(t, issue)
}

func TestJevShadow_LowConfidenceAndEscalationAreRecordedToo(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(f.confidentWorkerAnswer(0.55))
	issue := f.insertIssue(t, "todo")

	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
	if routed || advisory != "" {
		t.Fatalf("shadow low confidence: routed=%v advisory=%q, want false and empty (identical to off)", routed, advisory)
	}
	rec := logs.waitForShadowRecord(t, issue.ID)
	if rec["confidence"] != 0.55 || rec["outcome"] != "decision" {
		t.Errorf("record = %v, want outcome=decision confidence=0.55", rec)
	}
	f.assertNoJevSideEffects(t, issue)
}

func TestJevShadow_JevFailureIsLoggedAndNonFatal(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"http 500": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "quota exceeded", http.StatusInternalServerError)
		},
		"malformed answer": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(jev.Response{Model: "jev-1.13.0", Answers: map[string]jev.Answer{}})
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureSlog(t)
			f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
			f.serve(handler)
			issue := f.insertIssue(t, "todo")

			routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
			if routed || advisory != "" {
				t.Fatalf("Jev failure leaked into the leader path: routed=%v advisory=%q", routed, advisory)
			}
			rec := logs.waitForShadowRecord(t, issue.ID)
			if rec["outcome"] != "error" {
				t.Errorf("record[outcome] = %v, want error", rec["outcome"])
			}
			if msg, _ := rec["error"].(string); msg == "" {
				t.Error("record must carry the error text")
			}
			f.assertNoJevSideEffects(t, issue)
		})
	}
}

func TestJevShadow_MissingCredentialIsLoggedAndNonFatal(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(f.confidentWorkerAnswer(0.95))
	testHandler.Jev = nil
	issue := f.insertIssue(t, "todo")

	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
	if routed || advisory != "" {
		t.Fatalf("missing credential leaked into the leader path: routed=%v advisory=%q", routed, advisory)
	}
	rec := logs.waitForShadowRecord(t, issue.ID)
	if rec["outcome"] != "error" || !strings.Contains(fmt.Sprint(rec["error"]), "credential") {
		t.Errorf("record = %v, want outcome=error mentioning credential", rec)
	}
	f.assertNoJevSideEffects(t, issue)
}

// A shadow evaluation that cannot get a database answer must give up and log
// an error record instead of holding a pool connection forever (CHE-1745).
func TestJevShadow_BlockedDatabaseIsBoundedAndLoggedAsError(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(f.confidentWorkerAnswer(0.95))
	issue := f.insertIssue(t, "todo")

	ctx := context.Background()
	lockTx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(ctx) })
	if _, err := lockTx.Exec(ctx, `LOCK TABLE squad_member IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock squad_member: %v", err)
	}

	routed, advisory := testHandler.tryJevSquadRouting(ctx, issue, f.squad, "member", testUserID, testUserID)
	if routed || advisory != "" {
		t.Fatalf("blocked shadow evaluation leaked into the leader path: routed=%v advisory=%q", routed, advisory)
	}

	want := util.UUIDToString(issue.ID)
	deadline := time.Now().Add(12 * time.Second)
	var rec map[string]any
	for rec == nil && time.Now().Before(deadline) {
		for _, r := range logs.records() {
			if r["issue_id"] == want {
				rec = r
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec == nil {
		t.Fatal("blocked database: evaluation neither finished nor timed out within 12s; it still holds a pool connection")
	}
	if rec["outcome"] != "error" {
		t.Errorf("record[outcome] = %v, want error", rec["outcome"])
	}
}

// Replay needs the measured provider telemetry: the serving model, billed
// token usage and a hash of the exact input sent to Jev (CHE-1412 metrics).
func TestJevShadow_RecordCarriesServingModelUsageAndInputHash(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(f.confidentWorkerAnswer(0.95))
	issue := f.insertIssue(t, "todo")

	testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
	rec := logs.waitForShadowRecord(t, issue.ID)
	assertJevReplayTelemetry(t, rec)
}

// A billed call whose answer is unusable still cost tokens, so the error
// record keeps the usage and input hash too.
func TestJevShadow_MalformedAnswerRecordKeepsBilledUsage(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jev.Response{
			Model:   "jev-1.13.0",
			Answers: map[string]jev.Answer{},
			Usage:   jev.Usage{InputTokens: 500, OutputTokens: 10},
		})
	})
	issue := f.insertIssue(t, "todo")

	testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
	rec := logs.waitForShadowRecord(t, issue.ID)
	if rec["outcome"] != "error" {
		t.Fatalf("record[outcome] = %v, want error", rec["outcome"])
	}
	assertJevReplayTelemetry(t, rec)
}

// input_sha256 must identify the request Jev actually received, so it has to
// match the SHA-256 of the captured wire bytes, including the model the client
// fills in before sending.
func TestJevShadow_InputHashMatchesCapturedWireBytes(t *testing.T) {
	cases := map[string]func(f *jevShadowFixture) http.HandlerFunc{
		"decision": func(f *jevShadowFixture) http.HandlerFunc { return f.confidentWorkerAnswer(0.95) },
		"billed missing answer": func(f *jevShadowFixture) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(jev.Response{
					Model:   "jev-1.13.0",
					Answers: map[string]jev.Answer{},
					Usage:   jev.Usage{InputTokens: 500, OutputTokens: 10},
				})
			}
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureSlog(t)
			f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
			var mu sync.Mutex
			var wire []byte
			respond := answer(f)
			f.serve(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				wire = body
				mu.Unlock()
				respond(w, r)
			})
			issue := f.insertIssue(t, "todo")

			testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
			rec := logs.waitForShadowRecord(t, issue.ID)

			mu.Lock()
			defer mu.Unlock()
			if len(wire) == 0 {
				t.Fatal("mock Jev API captured no request body")
			}
			sum := sha256.Sum256(wire)
			if got, want := rec["input_sha256"], hex.EncodeToString(sum[:]); got != want {
				t.Errorf("record[input_sha256] = %v, want SHA-256 of the captured request bytes %s", got, want)
			}
		})
	}
}

func assertJevReplayTelemetry(t *testing.T, rec map[string]any) {
	t.Helper()
	for key, want := range map[string]any{
		"serving_model": "jev-1.13.0",
		"input_tokens":  float64(500),
		"output_tokens": float64(10),
	} {
		if rec[key] != want {
			t.Errorf("record[%s] = %v, want %v", key, rec[key], want)
		}
	}
	if sum, _ := rec["input_sha256"].(string); len(sum) != 64 {
		t.Errorf("record[input_sha256] = %v, want a 64-char hex digest", rec["input_sha256"])
	}
}

func TestJevShadow_AgentAuthoredCommentIsNotEvaluated(t *testing.T) {
	logs := captureSlog(t)
	var calls int
	var mu sync.Mutex
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "shadow"})
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		f.confidentWorkerAnswer(0.95)(w, r)
	})
	issue := f.insertIssue(t, "todo")

	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "agent", util.UUIDToString(f.workerID), "", dbid.NewV7())
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if routed || advisory != "" || calls != 0 || len(logs.records()) != 0 {
		t.Fatalf("agent-authored trigger must bypass Jev in shadow: routed=%v advisory=%q calls=%d records=%d", routed, advisory, calls, len(logs.records()))
	}
}

func TestJevRoutingMode_OnDispatchesWithoutLegacyFlag(t *testing.T) {
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_mode": "on"})
	f.serve(f.confidentWorkerAnswer(0.95))
	issue := f.insertIssue(t, "backlog")

	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
	if !routed {
		t.Fatalf("mode=on must dispatch, got routed=false advisory=%q", advisory)
	}
	var workerTasks int
	_ = testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issue.ID, f.workerID).Scan(&workerTasks)
	if workerTasks != 1 {
		t.Errorf("worker tasks = %d, want 1", workerTasks)
	}
}

func TestJevRoutingMode_LegacyEnabledFlagStillMeansOn(t *testing.T) {
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_enabled": true})
	f.serve(f.confidentWorkerAnswer(0.95))
	issue := f.insertIssue(t, "backlog")

	if routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID); !routed {
		t.Fatalf("legacy jev_squad_routing_enabled=true must keep dispatching, advisory=%q", advisory)
	}
}

func TestJevRoutingMode_ExplicitModeWinsOverLegacyFlag(t *testing.T) {
	logs := captureSlog(t)
	for _, tc := range []struct {
		mode       string
		wantRecord bool
	}{{"off", false}, {"shadow", true}} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_enabled": true, "jev_squad_routing_mode": tc.mode})
			f.serve(f.confidentWorkerAnswer(0.95))
			issue := f.insertIssue(t, "backlog")

			routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
			if routed || advisory != "" {
				t.Fatalf("mode=%s must not dispatch: routed=%v advisory=%q", tc.mode, routed, advisory)
			}
			if tc.wantRecord {
				logs.waitForShadowRecord(t, issue.ID)
			} else {
				time.Sleep(200 * time.Millisecond)
				for _, rec := range logs.records() {
					if rec["issue_id"] == util.UUIDToString(issue.ID) {
						t.Fatalf("mode=off must not log a shadow record: %v", rec)
					}
				}
			}
			f.assertNoJevSideEffects(t, issue)
		})
	}
}

func TestJevRoutingMode_UnknownValueFailsSafeToOff(t *testing.T) {
	logs := captureSlog(t)
	f := newJevShadowFixture(t, map[string]any{"jev_squad_routing_enabled": true, "jev_squad_routing_mode": "bogus"})
	f.serve(f.confidentWorkerAnswer(0.95))
	issue := f.insertIssue(t, "backlog")

	routed, advisory := testHandler.tryJevSquadRouting(context.Background(), issue, f.squad, "member", testUserID, testUserID)
	time.Sleep(200 * time.Millisecond)
	if routed || advisory != "" || len(logs.records()) != 0 {
		t.Fatalf("unknown mode must behave as off: routed=%v advisory=%q records=%d", routed, advisory, len(logs.records()))
	}
	f.assertNoJevSideEffects(t, issue)
}

func TestGovernanceConfigPatch_AcceptsRoutingMode(t *testing.T) {
	decode := func(body string) governanceConfigPatch {
		var patch governanceConfigPatch
		if err := json.Unmarshal([]byte(body), &patch); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		return patch
	}
	for _, mode := range []string{"off", "shadow", "on"} {
		patch := decode(fmt.Sprintf(`{"request_id":%q,"jev_squad_routing_mode":%q}`, uuid.NewString(), mode))
		if !validGovernanceConfigPatch(patch) {
			t.Fatalf("mode %q patch rejected", mode)
		}
		settings := governance.DefaultWorkspaceConfig().Settings
		applyGovernanceConfigPatch(&settings, patch)
		raw, _ := json.Marshal(settings)
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		if got["jev_squad_routing_mode"] != mode {
			t.Errorf("settings[jev_squad_routing_mode] = %v, want %q", got["jev_squad_routing_mode"], mode)
		}
	}
	// Another settings field makes the patch non-empty, so only the mode value can reject it.
	bad := decode(fmt.Sprintf(`{"request_id":%q,"jev_api_url":"https://jev.example","jev_squad_routing_mode":"bogus"}`, uuid.NewString()))
	if validGovernanceConfigPatch(bad) {
		t.Error("unknown routing mode was accepted")
	}
}

func TestGovernanceConfigPatch_LegacyEnabledFlagSetsMode(t *testing.T) {
	for enabled, want := range map[bool]string{true: "on", false: "off"} {
		var patch governanceConfigPatch
		body := fmt.Sprintf(`{"request_id":%q,"jev_squad_routing_enabled":%t}`, uuid.NewString(), enabled)
		if err := json.Unmarshal([]byte(body), &patch); err != nil {
			t.Fatalf("decode patch: %v", err)
		}
		settings := governance.DefaultWorkspaceConfig().Settings
		applyGovernanceConfigPatch(&settings, patch)
		raw, _ := json.Marshal(settings)
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		if got["jev_squad_routing_mode"] != want {
			t.Errorf("enabled=%v: settings[jev_squad_routing_mode] = %v, want %q", enabled, got["jev_squad_routing_mode"], want)
		}
	}
}

// serve sets the mock Jev API handler; tests build answers from fixture IDs.
func (f *jevShadowFixture) serve(h http.HandlerFunc) { f.handler.Store(h) }
