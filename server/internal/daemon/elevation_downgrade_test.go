package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Downgrade handshake, daemon half (plan 3.8, slice B4c; T26, T28 (a), (b)).
// The run is restricted locally first and stays restricted whatever the server
// does; the proxy gets nil only after the server confirmed delivery.

type trace struct {
	mu    sync.Mutex
	steps []string
}

func (t *trace) add(s string) { t.mu.Lock(); t.steps = append(t.steps, s); t.mu.Unlock() }
func (t *trace) get() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.steps...)
}

type fakeFence struct {
	tr    *trace
	err   error
	block bool
}

func (f fakeFence) Run(ctx context.Context) error {
	f.tr.add("fence")
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

type fakeDowngradeServer struct {
	tr          *trace
	startErrs   []error // one per attempt; nil past the end
	deliverErr  error
	startEpochs []int64
}

func (s *fakeDowngradeServer) StartDowngrade(ctx context.Context, epoch int64) error {
	s.tr.add("start")
	s.startEpochs = append(s.startEpochs, epoch)
	if n := len(s.startEpochs); n <= len(s.startErrs) {
		return s.startErrs[n-1]
	}
	return nil
}

func (s *fakeDowngradeServer) DeliverDowngrade(ctx context.Context) error {
	s.tr.add("deliver")
	return s.deliverErr
}

func newDowngrader(tr *trace, srv *fakeDowngradeServer, f fakeFence, alerts *[]error) *Downgrader {
	return &Downgrader{Server: srv, Fence: f, Epoch: 3, Attempts: 3, Backoff: time.Millisecond,
		Alert: func(err error) { *alerts = append(*alerts, err) }}
}

func TestDowngradeRestrictsLocallyThenFencesThenDelivers(t *testing.T) {
	tr := &trace{}
	srv := &fakeDowngradeServer{tr: tr}
	var alerts []error
	if err := newDowngrader(tr, srv, fakeFence{tr: tr}, &alerts).Downgrade(context.Background()); err != nil {
		t.Fatalf("Downgrade: %v", err)
	}
	if got, want := tr.get(), []string{"fence", "start", "deliver"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps %v, want %v", got, want)
	}
	if !reflect.DeepEqual(srv.startEpochs, []int64{3}) || len(alerts) != 0 {
		t.Fatalf("epochs %v alerts %v", srv.startEpochs, alerts)
	}
}

// T28 (a): the server fence call fails every time. The local fence already ran,
// nothing was delivered, and an alert went out.
func TestServerFenceFailureRestrictsLocallyAndDeliversNothing(t *testing.T) {
	tr := &trace{}
	boom := errors.New("HTTP 500")
	srv := &fakeDowngradeServer{tr: tr, startErrs: []error{boom, boom, boom, boom}}
	var alerts []error
	err := newDowngrader(tr, srv, fakeFence{tr: tr}, &alerts).Downgrade(context.Background())
	if err == nil {
		t.Fatal("Downgrade returned nil: bytes would be delivered")
	}
	if got, want := tr.get(), []string{"fence", "start", "start", "start"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps %v, want local fence then 3 start attempts and no deliver %v", got, want)
	}
	if len(alerts) == 0 {
		t.Fatal("no alert")
	}
}

func TestServerFenceRetrySucceeds(t *testing.T) {
	tr := &trace{}
	srv := &fakeDowngradeServer{tr: tr, startErrs: []error{errors.New("HTTP 503")}}
	var alerts []error
	if err := newDowngrader(tr, srv, fakeFence{tr: tr}, &alerts).Downgrade(context.Background()); err != nil {
		t.Fatalf("Downgrade: %v", err)
	}
	if got, want := tr.get(), []string{"fence", "start", "start", "deliver"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps %v, want %v", got, want)
	}
}

// T28 (b): confirmation never arrives: the local fence blocks until the proxy's
// deadline. No deliver call, an error, an alert.
func TestDaemonConfirmationTimeoutDeliversNothing(t *testing.T) {
	tr := &trace{}
	srv := &fakeDowngradeServer{tr: tr}
	var alerts []error
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := newDowngrader(tr, srv, fakeFence{tr: tr, block: true}, &alerts).Downgrade(ctx)
	if err == nil {
		t.Fatal("Downgrade returned nil after a timed-out fence")
	}
	for _, s := range tr.get() {
		if s == "deliver" {
			t.Fatalf("delivered after a failed local fence: %v", tr.get())
		}
	}
	if len(alerts) == 0 {
		t.Fatal("no alert")
	}
}

func TestServerRefusesDeliveryReturnsError(t *testing.T) {
	tr := &trace{}
	srv := &fakeDowngradeServer{tr: tr, deliverErr: errors.New("HTTP 409 fence not complete")}
	var alerts []error
	if err := newDowngrader(tr, srv, fakeFence{tr: tr}, &alerts).Downgrade(context.Background()); err == nil {
		t.Fatal("Downgrade returned nil though the server refused delivery")
	}
	if len(alerts) == 0 {
		t.Fatal("no alert")
	}
}

func TestDowngradeClientCallsThePlannedEndpoints(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/daemon/tasks/t1/elevation/downgrade":
			if body["epoch"] != float64(5) {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"state":"fencing","epoch":6}`))
		case "/api/daemon/tasks/t1/elevation/downgrade/deliver":
			_, _ = w.Write([]byte(`{"state":"restricted_final","epoch":6}`))
		case "/api/daemon/tasks/t2/elevation/downgrade/deliver":
			http.Error(w, `{"error":"fence not complete"}`, http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.SetToken("tok")
	if err := c.StartDowngrade(context.Background(), "t1", 5); err != nil {
		t.Fatalf("StartDowngrade: %v", err)
	}
	if err := c.DeliverDowngrade(context.Background(), "t1"); err != nil {
		t.Fatalf("DeliverDowngrade: %v", err)
	}
	if err := c.DeliverDowngrade(context.Background(), "t2"); err == nil {
		t.Fatal("a 409 was taken as success")
	}
	want := []string{"POST /api/daemon/tasks/t1/elevation/downgrade", "POST /api/daemon/tasks/t1/elevation/downgrade/deliver", "POST /api/daemon/tasks/t2/elevation/downgrade/deliver"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("requests %v, want %v", seen, want)
	}
}
