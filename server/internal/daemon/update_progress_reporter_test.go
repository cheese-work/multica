package daemon

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestUpdateProgressReporter_AtMostOncePerSecond(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	now := time.Unix(1000, 0)
	r := newUpdateProgressReporter(func(_ context.Context, p map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, p)
		return nil
	})
	r.now = func() time.Time { return now }

	for i := range 50 { // 49 more calls inside the same second
		r.report("downloading", int64(i), 100)
	}
	r.wait()
	now = now.Add(999 * time.Millisecond)
	r.report("downloading", 60, 100)
	r.wait()
	now = now.Add(time.Millisecond)
	r.report("downloading", 70, 100)
	r.wait()

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("sent %d reports, want 2 (t=0 and t=1s)", len(sent))
	}
	if got := sent[1]["progress"].(map[string]any)["bytes_done"]; got != int64(70) {
		t.Fatalf("second report bytes_done = %v, want 70", got)
	}
}
