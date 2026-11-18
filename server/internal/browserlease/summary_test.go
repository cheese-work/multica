package browserlease

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-4745 (W3-16; plan W1 r3, F6): the gateway posts one summary per ended lease to the internal
// listener. Install-token auth, write-once storage on the lease row, idempotent repeat.

const goodSummary = `{"epoch":3,"requests":120,"denials":4,"bytes_in":1048576,"bytes_out":2048,` +
	`"first_request_at":"2026-11-18T06:00:00Z","last_request_at":"2026-11-18T06:09:30Z"}`

func summaryPath(lease pgtype.UUID) string {
	return strings.Replace(SummaryPath, "{lease_id}", uuid.UUID(lease.Bytes).String(), 1)
}

func (e *ackEnv) summaryServer(t *testing.T, ok bool, ws pgtype.UUID) http.Handler {
	t.Helper()
	return NewSummaryService(e.pool).Routes(func(*http.Request) (pgtype.UUID, bool) { return ws, ok })
}

func (e *ackEnv) storedSummary(t *testing.T, lease pgtype.UUID) (raw []byte, at bool) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(),
		`SELECT summary, summary_at IS NOT NULL FROM browser_lease WHERE id = $1`, lease).Scan(&raw, &at); err != nil {
		t.Fatal(err)
	}
	return raw, at
}

func TestSummaryStoresOnEndedLease(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 3, "verifying")
	lease := e.lease(t, inst, "ended", 3)

	rec := post(e.summaryServer(t, true, e.ws), summaryPath(lease), goodSummary)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	raw, at := e.storedSummary(t, lease)
	if !at || raw == nil {
		t.Fatal("summary and summary_at must be stored")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["requests"] != float64(120) || got["denials"] != float64(4) || got["bytes_in"] != float64(1048576) {
		t.Errorf("stored summary = %s", raw)
	}
}

func TestSummaryRepeatIsIdempotentAndChangeIsRefused(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 3, "verifying")
	lease := e.lease(t, inst, "ended", 3)
	h := e.summaryServer(t, true, e.ws)

	if rec := post(h, summaryPath(lease), goodSummary); rec.Code != 200 {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, summaryPath(lease), goodSummary); rec.Code != 200 {
		t.Errorf("same body again: %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	changed := strings.Replace(goodSummary, `"requests":120`, `"requests":121`, 1)
	rec := post(h, summaryPath(lease), changed)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "summary_conflict") {
		t.Errorf("different body: %d %s, want 409 summary_conflict", rec.Code, rec.Body.String())
	}
	raw, _ := e.storedSummary(t, lease)
	if !strings.Contains(string(raw), `"requests": 120`) {
		t.Errorf("the first summary must stay: %s", raw)
	}
}

func TestSummaryStatusCodes(t *testing.T) {
	e := newAckEnv(t)
	inst := e.instance(t, 3, "verifying")
	ended := e.lease(t, inst, "ended", 3)
	inst2 := e.instance(t, 2, "active")
	active := e.lease(t, inst2, "active", 2)
	inst3 := e.instance(t, 0, "idle_clean")
	queued := e.lease(t, inst3, "queued", 0)
	h := e.summaryServer(t, true, e.ws)

	cases := []struct {
		name, path, body string
		handler          http.Handler
		want             int
		code             string
	}{
		{"unauthenticated", summaryPath(ended), goodSummary, e.summaryServer(t, false, pgtype.UUID{}), 401, "unauthorized"},
		{"other workspace", summaryPath(ended), goodSummary, e.summaryServer(t, true, pgUUID(uuid.New())), 404, "not_found"},
		{"unknown lease", summaryPath(pgUUID(uuid.New())), goodSummary, h, 404, "not_found"},
		{"bad lease id", strings.Replace(SummaryPath, "{lease_id}", "nope", 1), goodSummary, h, 400, "invalid_request"},
		{"active lease", summaryPath(active), strings.Replace(goodSummary, `"epoch":3`, `"epoch":2`, 1), h, 409, "lease_not_ended"},
		{"queued lease", summaryPath(queued), strings.Replace(goodSummary, `"epoch":3`, `"epoch":1`, 1), h, 409, "lease_not_ended"},
		{"stale epoch", summaryPath(ended), strings.Replace(goodSummary, `"epoch":3`, `"epoch":2`, 1), h, 409, "stale_epoch"},
		{"missing epoch", summaryPath(ended), `{"requests":1,"denials":0,"bytes_in":0,"bytes_out":0}`, h, 400, "invalid_request"},
		{"negative count", summaryPath(ended), strings.Replace(goodSummary, `"requests":120`, `"requests":-1`, 1), h, 400, "invalid_request"},
		{"unknown field", summaryPath(ended), strings.Replace(goodSummary, `"epoch":3`, `"epoch":3,"url":"https://x"`, 1), h, 400, "invalid_request"},
		{"bad time", summaryPath(ended), strings.Replace(goodSummary, `2026-11-18T06:00:00Z`, `yesterday`, 1), h, 400, "invalid_request"},
		{"last before first", summaryPath(ended), strings.Replace(goodSummary, `2026-11-18T06:09:30Z`, `2026-11-18T05:00:00Z`, 1), h, 400, "invalid_request"},
		{"bad json", summaryPath(ended), `{`, h, 400, "invalid_request"},
		{"too large", summaryPath(ended), `{"epoch":3,"pad":"` + strings.Repeat("a", 8<<10) + `"}`, h, 400, "invalid_request"},
	}
	for _, tc := range cases {
		rec := post(tc.handler, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			continue
		}
		var out struct{ Error string }
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Error != tc.code {
			t.Errorf("%s: error %q, want %q", tc.name, out.Error, tc.code)
		}
	}
	for _, l := range []pgtype.UUID{ended, active, queued} {
		if raw, at := e.storedSummary(t, l); raw != nil || at {
			t.Errorf("a refused request must store nothing, lease %x has %s", l.Bytes[:4], raw)
		}
	}
}

func TestSummaryRoutesNeedAnAuthenticator(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Mount(nil) must panic: never serve an unauthenticated state-changing route")
		}
	}()
	NewSummaryService(nil).Mount(nil)
}
