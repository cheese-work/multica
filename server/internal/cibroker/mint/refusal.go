package mint

import (
	"fmt"
	"time"
)

// Refusal is a request the broker will not serve. Status is the HTTP status.
type Refusal struct {
	Status     int
	Reason     string
	RetryAfter time.Duration
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("ci broker refused (%d): %s", r.Status, r.Reason)
}
