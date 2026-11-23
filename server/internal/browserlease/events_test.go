package browserlease

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	instA = "11111111-1111-1111-1111-111111111111"
	instB = "22222222-2222-2222-2222-222222222222"
	tokC  = "mbc_c00"
	tokX  = "mbc_x99"
)

type fakeStore struct {
	mu     sync.Mutex
	live   map[string][]LiveLease
	leases map[string]LeaseInfo // key instance/lease
	err    error
}

func (f *fakeStore) LiveLeases(_ context.Context, id string) ([]LiveLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]LiveLease(nil), f.live[id]...), nil
}

func (f *fakeStore) Lease(_ context.Context, inst, lease string) (LeaseInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return LeaseInfo{}, false, f.err
	}
	l, ok := f.leases[inst+"/"+lease]
	return l, ok, nil
}

func (f *fakeStore) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

// c00 owns instA, x99 owns instB.
func newGateway(store Store, hb time.Duration) (*Gateway, http.Handler) {
	g := &Gateway{
		Store:     store,
		Broker:    &Broker{},
		Heartbeat: hb,
		Authenticate: func(r *http.Request) (Principal, bool) {
			switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
			case tokC:
				return Principal{Gateway: "c00"}, true
			case tokX:
				return Principal{Gateway: "x99"}, true
			}
			return Principal{}, false
		},
		Owns: func(_ context.Context, p Principal, inst string) (bool, error) {
			return (p.Gateway == "c00" && inst == instA) || (p.Gateway == "x99" && inst == instB), nil
		},
	}
	r := chi.NewRouter()
	r.Method(http.MethodGet, EventsPath, g.EventsHandler())
	r.Method(http.MethodGet, LeasePath, g.LeaseHandler())
	return g, r
}

func get(h http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type sseEvent struct {
	name string
	data string
}

// openStream starts a real server and returns a channel of parsed SSE events.
func openStream(t *testing.T, h http.Handler, inst, token string) (<-chan sseEvent, *http.Response, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/internal/browser/instances/"+inst+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan sseEvent, 64)
	if resp.StatusCode != http.StatusOK {
		close(out)
		return out, resp, cancel
	}
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if ev.name != "" {
					out <- ev
				}
				ev = sseEvent{}
			}
		}
	}()
	return out, resp, cancel
}

func next(t *testing.T, ch <-chan sseEvent, within time.Duration) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %v", within)
	}
	return sseEvent{}
}

func TestDefaultHeartbeatIsTwoSeconds(t *testing.T) {
	if DefaultHeartbeat != 2*time.Second {
		t.Fatalf("DefaultHeartbeat = %v, want 2s", DefaultHeartbeat)
	}
}

func TestHeartbeatConfirmsEveryLiveLeaseAndRepeats(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	store := &fakeStore{live: map[string][]LiveLease{instA: {
		{LeaseID: "l1", Epoch: 3, ExpiresAt: exp},
		{LeaseID: "l2", Epoch: 4, ExpiresAt: exp},
		{LeaseID: "gone", Epoch: 2, ExpiresAt: time.Now().Add(-time.Minute)}, // expired: not confirmed
	}}}
	_, h := newGateway(store, 30*time.Millisecond)
	ch, _, _ := openStream(t, h, instA, tokC)
	for i := 0; i < 3; i++ { // first one at connect, then on every tick
		ev := next(t, ch, time.Second)
		if ev.name != "heartbeat" {
			t.Fatalf("event %d = %q, want heartbeat", i, ev.name)
		}
		var body struct {
			InstanceID string      `json:"instance_id"`
			Leases     []LiveLease `json:"leases"`
		}
		if err := json.Unmarshal([]byte(ev.data), &body); err != nil {
			t.Fatal(err)
		}
		if body.InstanceID != instA || len(body.Leases) != 2 || body.Leases[0].LeaseID != "l1" || body.Leases[1].LeaseID != "l2" {
			t.Fatalf("heartbeat %d = %+v", i, body)
		}
	}
}

func TestHeartbeatWithNoLiveLeaseHasEmptyArray(t *testing.T) {
	_, h := newGateway(&fakeStore{}, 30*time.Millisecond)
	ch, _, _ := openStream(t, h, instA, tokC)
	if ev := next(t, ch, time.Second); !strings.Contains(ev.data, `"leases":[]`) {
		t.Fatalf("data = %s, want empty leases array", ev.data)
	}
}

func TestPublishedEventArrivesAtOnce(t *testing.T) {
	g, h := newGateway(&fakeStore{}, time.Hour) // heartbeat too slow to be the carrier
	ch, _, _ := openStream(t, h, instA, tokC)
	next(t, ch, time.Second) // connect heartbeat
	g.Broker.Publish(instB, Event{Name: "revoked", Data: map[string]string{"lease_id": "other"}})
	g.Broker.Publish(instA, Event{Name: "revoked", Data: map[string]string{"lease_id": "l1"}})
	ev := next(t, ch, 500*time.Millisecond)
	if ev.name != "revoked" || !strings.Contains(ev.data, `"l1"`) {
		t.Fatalf("event = %+v, want revoked l1 (and nothing from instance B)", ev)
	}
}

func TestStoreErrorEndsStreamWithoutConfirming(t *testing.T) {
	store := &fakeStore{}
	_, h := newGateway(store, 30*time.Millisecond)
	ch, _, _ := openStream(t, h, instA, tokC)
	next(t, ch, time.Second)
	store.setErr(errors.New("db down"))
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // stream ended: no heartbeat confirms anything
			}
		case <-deadline:
			t.Fatal("stream still open after store error")
		}
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	b := &Broker{}
	ch, cancel := b.Subscribe(instA)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(instA, Event{Name: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a subscriber that does not read")
	}
	n := 0
	for range ch { // closed after the drop
		n++
	}
	if n == 0 || n >= 1000 {
		t.Fatalf("drained %d events", n)
	}
}

func TestAuthRefusals(t *testing.T) {
	_, h := newGateway(&fakeStore{leases: map[string]LeaseInfo{}}, time.Hour)
	for _, path := range []string{
		"/internal/browser/instances/" + instA + "/events",
		"/internal/browser/instances/" + instA + "/leases/l1",
	} {
		if w := get(h, path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s no token: %d, want 401", path, w.Code)
		}
		if w := get(h, path, "mpc_callback"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s non-install token: %d, want 401", path, w.Code)
		}
		if w := get(h, path, "mbc_unknown"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s unknown token: %d, want 401", path, w.Code)
		}
	}
}

// W3-18: a gateway credential works only for its own instances.
func TestGatewayCredentialIsScopedToItsInstances(t *testing.T) {
	store := &fakeStore{leases: map[string]LeaseInfo{instA + "/l1": {LeaseID: "l1", State: "active"}, instB + "/l9": {LeaseID: "l9", State: "active"}}}
	_, h := newGateway(store, time.Hour)
	cases := []struct {
		path, token string
		want        int
	}{
		{"/internal/browser/instances/" + instA + "/events", tokX, http.StatusNotFound}, // x99 on c00's instance
		{"/internal/browser/instances/" + instB + "/events", tokC, http.StatusNotFound}, // c00 on x99's instance
		{"/internal/browser/instances/" + instA + "/leases/l1", tokX, http.StatusNotFound},
		{"/internal/browser/instances/" + instB + "/leases/l9", tokC, http.StatusNotFound},
		{"/internal/browser/instances/" + instA + "/leases/l1", tokC, http.StatusOK},
		{"/internal/browser/instances/" + instB + "/leases/l9", tokX, http.StatusOK},
	}
	for _, c := range cases {
		w := get(h, c.path, c.token)
		if w.Code != c.want {
			t.Errorf("%s with %s: %d, want %d", c.path, c.token, w.Code, c.want)
		}
		if c.want == http.StatusNotFound && strings.Contains(w.Body.String(), "lease_id") {
			t.Errorf("%s leaked a body: %s", c.path, w.Body)
		}
	}
	// Streams too, over a real connection.
	_, resp, _ := openStream(t, h, instA, tokX)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign stream status = %d, want 404", resp.StatusCode)
	}
}

func TestOwnsErrorFailsClosed(t *testing.T) {
	g, h := newGateway(&fakeStore{}, time.Hour)
	g.Owns = func(context.Context, Principal, string) (bool, error) { return true, errors.New("registry down") }
	if w := get(h, "/internal/browser/instances/"+instA+"/leases/l1", tokC); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestBadInstanceIDIs404(t *testing.T) {
	_, h := newGateway(&fakeStore{}, time.Hour)
	if w := get(h, "/internal/browser/instances/not-a-uuid/events", tokC); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestIntrospectionReportsLiveness(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-time.Minute)
	store := &fakeStore{leases: map[string]LeaseInfo{
		instA + "/live":    {LeaseID: "live", State: "active", Epoch: 5, ExpiresAt: now.Add(time.Hour)},
		instA + "/expired": {LeaseID: "expired", State: "active", Epoch: 4, ExpiresAt: now.Add(-time.Second)},
		instA + "/ended":   {LeaseID: "ended", State: "ended", Epoch: 3, ExpiresAt: now.Add(time.Hour), EndReason: "revoked", EndedAt: &ended},
		instA + "/queued":  {LeaseID: "queued", State: "queued", ExpiresAt: now.Add(time.Hour)},
	}}
	g, h := newGateway(store, time.Hour)
	g.Now = func() time.Time { return now }
	for id, wantLive := range map[string]bool{"live": true, "expired": false, "ended": false, "queued": false} {
		w := get(h, "/internal/browser/instances/"+instA+"/leases/"+id, tokC)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", id, w.Code)
		}
		var got LeaseInfo
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Live != wantLive || got.LeaseID != id {
			t.Errorf("%s: %+v, want live=%v", id, got, wantLive)
		}
	}
	if w := get(h, "/internal/browser/instances/"+instA+"/leases/ended", tokC); !strings.Contains(w.Body.String(), `"end_reason":"revoked"`) {
		t.Errorf("ended body = %s", w.Body)
	}
	if w := get(h, "/internal/browser/instances/"+instA+"/leases/missing", tokC); w.Code != http.StatusNotFound {
		t.Errorf("missing lease: %d, want 404", w.Code)
	}
	if w := get(h, "/internal/browser/instances/"+instA+"/leases/live", tokC); w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}
}
