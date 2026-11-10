package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2673 L2: the daemon mirrors the server's pause from heartbeat acks,
// stops claiming while it holds one, and reports its in-flight count.

func pausedUntil(d time.Duration) *protocol.DaemonHeartbeatPaused {
	now := time.Now().UTC()
	return &protocol.DaemonHeartbeatPaused{
		Reason:    "maintenance",
		PausedAt:  now.Format(time.RFC3339),
		ExpiresAt: now.Add(d).Format(time.RFC3339),
	}
}

func TestSendHeartbeat_ReportsInFlightTasks(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "paused": pausedUntil(time.Hour)})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	resp, err := c.SendHeartbeat(context.Background(), protocol.DaemonHeartbeatRequestPayload{RuntimeID: "rt-1", InFlightTasks: new(int)})
	if err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
	// 0 is a real answer ("idle"), so it must be present, not omitted.
	if v, ok := got["in_flight_tasks"]; !ok || v != float64(0) {
		t.Fatalf("in_flight_tasks = %v (present %v), want 0 present", v, ok)
	}
	if resp.Paused == nil || resp.Paused.Reason != "maintenance" {
		t.Fatalf("ack paused = %+v, want maintenance", resp.Paused)
	}
}

func TestSendWSHeartbeats_ReportsInFlightTasks(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	d.activeTasks.Store(3)
	writes := make(chan *wsOutbound, 1)
	d.sendWSHeartbeats(context.Background(), []string{"rt-1"}, writes)

	var msg protocol.Message
	if err := json.Unmarshal((<-writes).data, &msg); err != nil {
		t.Fatal(err)
	}
	var payload protocol.DaemonHeartbeatRequestPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.InFlightTasks == nil || *payload.InFlightTasks != 3 {
		t.Fatalf("ws in_flight_tasks = %v, want 3", payload.InFlightTasks)
	}
}

func TestHeartbeatAck_PauseGatesClaimsUntilCleared(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	if !d.tryEnterClaim() {
		t.Fatal("claim refused with no pause")
	}
	d.exitClaim()

	d.handleHeartbeatActions(context.Background(), "rt-1", &HeartbeatResponse{Paused: pausedUntil(time.Hour)})
	if d.tryEnterClaim() {
		t.Fatal("claim allowed while the server reports a pause")
	}

	d.handleHeartbeatActions(context.Background(), "rt-1", &HeartbeatResponse{})
	if !d.tryEnterClaim() {
		t.Fatal("claim still refused after an ack without a pause")
	}
	d.exitClaim()
}

func TestHeartbeatAck_ExpiredPauseDoesNotGateClaims(t *testing.T) {
	d := &Daemon{logger: slog.Default()}
	d.handleHeartbeatActions(context.Background(), "rt-1", &HeartbeatResponse{Paused: pausedUntil(-time.Minute)})
	if !d.tryEnterClaim() {
		t.Fatal("claim refused by a pause whose expires_at has passed")
	}
	d.exitClaim()
}

func TestHealthHandlerReportsPause(t *testing.T) {
	d := &Daemon{
		cfg:        Config{CLIVersion: "v1.0.0"},
		workspaces: map[string]*workspaceState{},
		logger:     slog.Default(),
	}
	read := func() map[string]any {
		rec := httptest.NewRecorder()
		d.healthHandler(time.Now()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	if _, present := read()["paused"]; present {
		t.Fatal("health reports paused on an unpaused daemon")
	}
	p := pausedUntil(time.Hour)
	d.handleHeartbeatActions(context.Background(), "rt-1", &HeartbeatResponse{Paused: p})
	raw := read()
	if raw["paused"] != true || raw["pause_reason"] != "maintenance" || raw["pause_expires_at"] != p.ExpiresAt {
		t.Fatalf("health pause fields = %v %v %v", raw["paused"], raw["pause_reason"], raw["pause_expires_at"])
	}
}
