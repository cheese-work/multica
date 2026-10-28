package daemon

import (
	"context"
	"errors"
	"sync"
)

// inflightClaims tracks the tasks this daemon is executing, keyed by task id,
// with the newest claim generation (dispatched_at) the server has delivered.
//
// The server re-delivers a dispatched task whose prepare lease lapsed, under
// the same id and a refreshed generation. The execution already holding the
// task is the same work, so the repeat delivery must not start a second one
// (it would collide on the env root lock and fail the task). The refreshed
// generation is kept so the original execution's StartTask presents the claim
// the server now considers current instead of being rejected as stale.
// The zero value is ready to use.
type inflightClaims struct {
	mu      sync.Mutex
	entries map[string]*inflightClaim
}

// inflightClaim is one execution's hold on a task id. An abandoned hold no
// longer suppresses redelivery: its execution is ending without having started.
type inflightClaim struct {
	generation string
	abandoned  bool
}

// begin registers an execution of taskID. It returns nil when a live execution
// already holds the task, after recording the newer generation for it.
func (c *inflightClaims) begin(taskID, generation string) *inflightClaim {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held := c.entries[taskID]; held != nil && !held.abandoned {
		if generation != "" {
			held.generation = generation
		}
		return nil
	}
	if c.entries == nil {
		c.entries = make(map[string]*inflightClaim)
	}
	entry := &inflightClaim{generation: generation}
	c.entries[taskID] = entry
	return entry
}

// end releases entry; a later execution of the same task id is left alone.
func (c *inflightClaims) end(taskID string, entry *inflightClaim) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[taskID] == entry {
		delete(c.entries, taskID)
	}
}

func (c *inflightClaims) generation(taskID, fallback string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held := c.entries[taskID]; held != nil && !held.abandoned && held.generation != "" {
		return held.generation
	}
	return fallback
}

// supersededBy reports whether a duplicate delivery replaced the generation
// sent. When none did, the hold is abandoned in the same critical section, so
// a duplicate arriving after the check starts a fresh execution instead of
// being ignored while this one dies.
func (c *inflightClaims) supersededBy(taskID, sent string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.entries[taskID]
	if held == nil || held.abandoned {
		return false
	}
	if held.generation != sent {
		return true
	}
	held.abandoned = true
	return false
}

// latestClaimGeneration returns the claim generation StartTask must present.
func (d *Daemon) latestClaimGeneration(task Task) string {
	return d.inflight.generation(task.ID, task.DispatchedAt)
}

// startTaskWithCurrentClaim acknowledges the start with the newest claim. A
// duplicate delivery can refresh the claim while the start is in flight, so a
// rejection is retried with the refreshed generation.
func (d *Daemon) startTaskWithCurrentClaim(ctx context.Context, task Task, capabilities ...string) (TaskNegotiation, error) {
	for {
		task.DispatchedAt = d.latestClaimGeneration(task)
		negotiated, err := d.client.StartTask(ctx, task, capabilities...)
		if errors.Is(err, errStartClaimRejected) && d.inflight.supersededBy(task.ID, task.DispatchedAt) {
			continue
		}
		return negotiated, err
	}
}
