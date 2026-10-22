package daemon

import "sync"

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
	mu          sync.Mutex
	generations map[string]string
}

// begin registers taskID; it reports false when an execution already holds it,
// in which case the newer generation replaces the stored one.
func (c *inflightClaims) begin(taskID, generation string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.generations[taskID]; held {
		if generation != "" {
			c.generations[taskID] = generation
		}
		return false
	}
	if c.generations == nil {
		c.generations = make(map[string]string)
	}
	c.generations[taskID] = generation
	return true
}

func (c *inflightClaims) end(taskID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.generations, taskID)
}

func (c *inflightClaims) generation(taskID, fallback string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if g := c.generations[taskID]; g != "" {
		return g
	}
	return fallback
}

// latestClaimGeneration returns the claim generation StartTask must present.
func (d *Daemon) latestClaimGeneration(task Task) string {
	return d.inflight.generation(task.ID, task.DispatchedAt)
}
