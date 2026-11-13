package handler

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Every issue:updated event that moves a status must name the status row its
// writer committed (activity_id): the activity listener writes none (CHE-2854,
// C1d). watchStatusEventsWithoutActivityID records the events that do not, and
// TestMain fails the package run when any test produced one.
var (
	lostStatusMu     sync.Mutex
	lostStatusEvents []string
)

func watchStatusEventsWithoutActivityID(bus *events.Bus) {
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		if changed, _ := p["status_changed"].(bool); !changed {
			return
		}
		if id, _ := p["activity_id"].(string); id != "" {
			return
		}
		// A live duplicate mark change is named by the listener's duplicate
		// row, which stands in for the status row (MUL-7349).
		if fmt.Sprint(deref(p["duplicate_of_issue_id"])) != fmt.Sprint(deref(p["prev_duplicate_of_issue_id"])) {
			return
		}
		lostStatusMu.Lock()
		defer lostStatusMu.Unlock()
		source, _ := p["source"].(string)
		lostStatusEvents = append(lostStatusEvents, fmt.Sprintf("source=%q prev_status=%v to=%v by %s", source, p["prev_status"], issueStatus(p["issue"]), publisherFrames()))
	})
}

func statusEventsWithoutActivityID() []string {
	lostStatusMu.Lock()
	defer lostStatusMu.Unlock()
	return append([]string(nil), lostStatusEvents...)
}

func issueStatus(v any) string {
	if r, ok := v.(IssueResponse); ok {
		return r.Status
	}
	return "?"
}

// publisherFrames names the first frames outside the bus that led to the
// publish, so a hit points at the writer that forgot activity_id.
func publisherFrames() string {
	pcs := make([]uintptr, 24)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	var names []string
	for len(names) < 5 {
		f, more := frames.Next()
		if i := strings.LastIndex(f.Function, "/"); i >= 0 && !strings.Contains(f.Function, "events.") {
			names = append(names, f.Function[i+1:])
		}
		if !more {
			break
		}
	}
	return strings.Join(names, " < ")
}

func deref(v any) any {
	if s, ok := v.(*string); ok && s != nil {
		return *s
	}
	return nil
}
