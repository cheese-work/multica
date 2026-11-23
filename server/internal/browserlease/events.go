package browserlease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Gateway-facing routes (CHE-4735, W3-6; plan W1 r5, "Authorization deadline").
// They are served only on the internal listener (W3-17 mounts them).
const (
	EventsPath = "/internal/browser/instances/{id}/events"
	LeasePath  = "/internal/browser/instances/{id}/leases/{lease_id}"
)

// DefaultHeartbeat is the plan's 2 s heartbeat; the gateway's deadline is 5 s.
const DefaultHeartbeat = 2 * time.Second

// LiveLease is one granted, unended lease.
type LiveLease struct {
	LeaseID   string    `json:"lease_id"`
	Epoch     int64     `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LeaseInfo is what introspection reports about one lease.
type LeaseInfo struct {
	LeaseID   string     `json:"lease_id"`
	State     string     `json:"state"`
	Epoch     int64      `json:"epoch"`
	ExpiresAt time.Time  `json:"expires_at"`
	EndReason string     `json:"end_reason,omitempty"`
	Live      bool       `json:"live"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Event is a lease event pushed to a gateway at once (revoke, grant, ...).
type Event struct {
	Name string
	Data any
}

// Store reads lease state for one instance.
type Store interface {
	// LiveLeases returns the active leases of the instance.
	LiveLeases(ctx context.Context, instanceID string) ([]LiveLease, error)
	// Lease returns one lease of the instance; found is false when it does not exist there.
	Lease(ctx context.Context, instanceID, leaseID string) (info LeaseInfo, found bool, err error)
}

// Gateway serves the event stream and introspection routes.
type Gateway struct {
	Store     Store
	Broker    *Broker
	Heartbeat time.Duration // zero means DefaultHeartbeat
	// Authenticate resolves the request's gateway credential to the calling gateway (A1b). A
	// plugin install token is not a credential and resolves to nothing.
	Authenticate func(r *http.Request) (Principal, bool)
	// Owns reports whether the instance belongs to the credential's own gateway and workspace.
	Owns func(ctx context.Context, p Principal, instanceID string) (bool, error)
	Now  func() time.Time
}

// subBuffer bounds how far a stream may lag; past it the stream is dropped, never waited on.
const subBuffer = 16

// Broker fans events out to the streams open on an instance. Publish never blocks: a
// subscriber that does not keep up is closed and the gateway reconnects (it already treats a
// missing heartbeat as an outage).
type Broker struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

// Subscribe returns the instance's event channel and a cancel func. The channel is closed when
// the subscriber is dropped.
func (b *Broker) Subscribe(instanceID string) (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[string]map[chan Event]struct{}{}
	}
	if b.subs[instanceID] == nil {
		b.subs[instanceID] = map[chan Event]struct{}{}
	}
	b.subs[instanceID][ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() { b.drop(instanceID, ch) }
}

func (b *Broker) drop(instanceID string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[instanceID][ch]; ok {
		delete(b.subs[instanceID], ch)
		close(ch)
	}
}

// Publish sends ev to every stream open on the instance.
func (b *Broker) Publish(instanceID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[instanceID] {
		select {
		case ch <- ev:
		default:
			delete(b.subs[instanceID], ch)
			close(ch)
		}
	}
}

func (g *Gateway) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// authorize returns the instance id when the caller holds a gateway credential bound to it. It
// writes the refusal itself otherwise. An unknown, malformed or foreign instance (another
// gateway's or another workspace's) is one 404, so a gateway cannot probe another host's instances.
func (g *Gateway) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, ok := g.Authenticate(r)
	if !ok {
		http.Error(w, "gateway credential required", http.StatusUnauthorized)
		return "", false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.NotFound(w, r)
		return "", false
	}
	owns, err := g.Owns(r.Context(), p, id.String())
	switch {
	case err != nil:
		http.Error(w, "instance registry unavailable", http.StatusServiceUnavailable)
		return "", false
	case !owns:
		http.NotFound(w, r)
		return "", false
	}
	return id.String(), true
}

// EventsHandler streams lease events as SSE: events at once, and a heartbeat at connect and
// then every Heartbeat that lists every live lease on the instance. The gateway treats 5 s
// without a heartbeat as an outage, so any error here ends the stream rather than sending a
// heartbeat that confirms nothing.
func (g *Gateway) EventsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instanceID, ok := g.authorize(w, r)
		if !ok {
			return
		}
		interval := g.Heartbeat
		if interval <= 0 {
			interval = DefaultHeartbeat
		}
		rc := http.NewResponseController(w)
		events, cancel := g.Broker.Subscribe(instanceID)
		defer cancel()

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		send := func(name string, data any) bool {
			body, err := json.Marshal(data)
			if err != nil {
				return false
			}
			// A server WriteTimeout would cut a long-lived stream; extend it per write.
			_ = rc.SetWriteDeadline(time.Now().Add(5 * interval))
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
				return false
			}
			return rc.Flush() == nil
		}
		heartbeat := func() bool {
			live, err := g.Store.LiveLeases(r.Context(), instanceID)
			if err != nil {
				return false
			}
			now := g.now()
			leases := make([]LiveLease, 0, len(live))
			for _, l := range live {
				if now.Before(l.ExpiresAt) {
					leases = append(leases, l)
				}
			}
			return send("heartbeat", map[string]any{"instance_id": instanceID, "at": now, "leases": leases})
		}

		if !heartbeat() {
			return
		}
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, open := <-events:
				if !open || !send(ev.Name, ev.Data) {
					return
				}
			case <-tick.C:
				if !heartbeat() {
					return
				}
			}
		}
	})
}

// LeaseHandler reports one lease of the instance and whether it is live now.
func (g *Gateway) LeaseHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instanceID, ok := g.authorize(w, r)
		if !ok {
			return
		}
		info, found, err := g.Store.Lease(r.Context(), instanceID, chi.URLParam(r, "lease_id"))
		switch {
		case err != nil:
			http.Error(w, "lease store unavailable", http.StatusServiceUnavailable)
			return
		case !found:
			http.NotFound(w, r)
			return
		}
		info.Live = info.State == "active" && g.now().Before(info.ExpiresAt)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(info)
	})
}
