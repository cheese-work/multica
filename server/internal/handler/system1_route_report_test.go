package handler

import (
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-4550 (S6 of CHE-4439, spec decision 23): the shadow report's `route`
// block. Coverage and adoption use one event population: agent status_changed
// rows marked routing_exit, joined to route rows by event id.

const (
	reportSince = "2026-01-10T00:00:00+07:00"
	reportUntil = "2026-01-11T00:00:00+07:00"
)

type routeReport struct {
	Window struct {
		Since string `json:"since"`
		Until string `json:"until"`
	} `json:"window"`
	EligibleExits    int64                `json:"eligible_exits"`
	MatchedLabels    int64                `json:"matched_labels"`
	LabelCoverage    float64              `json:"label_coverage"`
	ViaRouteShare    float64              `json:"via_route_share"`
	DuplicateLabels  int64                `json:"duplicate_labels"`
	LabelsTotal      int64                `json:"labels_total"`
	OwnerOnlyLabels  int64                `json:"owner_only_labels"`
	NonRoutingLabels int64                `json:"non_routing_labels"`
	Overridden       int64                `json:"overridden"`
	OverrideRate     float64              `json:"override_rate"`
	Decided          int64                `json:"decided"`
	Classes          []system1ClassReport `json:"classes"`
}

func getRouteReport(t *testing.T, query string) routeReport {
	t.Helper()
	var body struct {
		Route routeReport `json:"route"`
	}
	testutil.Call(t, testHandler.GetSystem1Report, newRequest(http.MethodGet, "/api/decisions/shadow-report"+query, nil)).Want(http.StatusOK).JSON(&body)
	return body.Route
}

func windowQuery() string {
	return "?since=" + url.QueryEscape(reportSince) + "&until=" + url.QueryEscape(reportUntil)
}

func ratioIs(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

type reportSeed struct {
	t     *testing.T
	issue string
}

func newReportSeed(t *testing.T) reportSeed {
	t.Helper()
	s := reportSeed{t: t, issue: dbfx.Issue(t, "route report issue")}
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM system1_observation WHERE subject_id = $1`, s.issue)
		dbfx.Exec(t, `DELETE FROM activity_log WHERE issue_id = $1`, s.issue)
	})
	return s
}

// exit seeds a status_changed row by an agent and returns its id, the event id.
func (s reportSeed) exit(at string, routingExit bool) string {
	s.t.Helper()
	id := uuidToString(dbid.NewV7())
	details := `{"from":"todo","to":"in_progress"}`
	if routingExit {
		details = `{"from":"todo","to":"in_progress","routing_exit":true}`
	}
	dbfx.Exec(s.t, `INSERT INTO activity_log (id, workspace_id, issue_id, actor_type, action, details, created_at) VALUES ($1, $2, $3, 'agent', 'status_changed', $4::jsonb, $5::timestamptz)`,
		id, testWorkspaceID, s.issue, details, at)
	return id
}

type labelOpt struct {
	at, class, source, via string
	event                  any // event id, or nil for an owner-only route
	nonRouting             bool
	predicted              string
	reversed               bool
}

const routeChoice = "in_progress|agent:a"

func (s reportSeed) label(o labelOpt) {
	s.t.Helper()
	if o.class == "" {
		o.class = "wf/todo"
	}
	if o.source == "" {
		o.source = "agent"
	}
	if o.via == "" {
		o.via = "route"
	}
	if o.predicted == "" {
		o.predicted = routeChoice
	}
	inputs := `{"via":"` + o.via + `","routing_exit":` + map[bool]string{true: "false", false: "true"}[o.nonRouting] + `}`
	dbfx.Exec(s.t, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, decision_source, inputs, predicted_key, choice_key, outcome, event_id, created_at, reversed_at)
		VALUES ($1, 'route', $2, $3, $4, $5::jsonb, $6, $7, 'applied', $8::uuid, $9::timestamptz, CASE WHEN $10::boolean THEN $9::timestamptz + interval '1 minute' END)`,
		testWorkspaceID, s.issue, o.class, o.source, inputs, o.predicted, routeChoice, o.event, o.at, o.reversed)
}

// Every named case of the spec's report test list is in this one seed.
func TestRouteReportEventMatchedNumbers(t *testing.T) {
	s := newReportSeed(t)
	in := "2026-01-10T12:00:00+07:00"

	e1 := s.exit(in, true) // matched, labelled by `issue route`, then overridden
	s.label(labelOpt{at: in, event: e1, reversed: true})
	e2 := s.exit(in, true) // matched, labelled by the status fallback; the predictor disagrees
	s.label(labelOpt{at: in, event: e2, via: "status", predicted: "blocked|agent:a"})
	e3 := s.exit(in, true) // two rows share the event: the earliest is the fallback, the later is a duplicate
	s.label(labelOpt{at: "2026-01-10T12:00:01+07:00", event: e3, via: "status"})
	s.label(labelOpt{at: "2026-01-10T12:00:02+07:00", event: e3})
	s.exit(in, true) // a fallback capture failure: 1 eligible exit, 0 matched
	// One task leaves the same source step twice: 2 eligible exits, 2 matched, 0 duplicates.
	e5 := s.exit("2026-01-10T13:00:00+07:00", true)
	s.label(labelOpt{at: "2026-01-10T13:00:00+07:00", event: e5})
	e6 := s.exit("2026-01-10T14:00:00+07:00", true)
	s.label(labelOpt{at: "2026-01-10T14:00:00+07:00", event: e6})

	// A workflow override disabled routing for this step, so the exit is not eligible.
	// Its route comes from a step that is not a routing step and is counted apart.
	e7 := s.exit(in, false)
	s.label(labelOpt{at: in, event: e7, class: "wf/in_progress", nonRouting: true})
	s.label(labelOpt{at: in, event: nil, nonRouting: true})                  // owner-only: no status change, no event
	s.label(labelOpt{at: in, event: nil, source: "human", nonRouting: true}) // a human row is no agent label

	// Outside the window: before `since`, and at `until` (exclusive).
	before := s.exit("2026-01-09T23:59:59+07:00", true)
	s.label(labelOpt{at: "2026-01-09T23:59:59+07:00", event: before})
	atUntil := s.exit(reportUntil, true)
	s.label(labelOpt{at: reportUntil, event: atUntil})

	got := getRouteReport(t, windowQuery())
	if got.EligibleExits != 6 || got.MatchedLabels != 5 {
		t.Fatalf("eligible_exits=%d matched_labels=%d, want 6 and 5", got.EligibleExits, got.MatchedLabels)
	}
	if !ratioIs(got.LabelCoverage, 5.0/6) {
		t.Errorf("label_coverage = %v, want 5/6", got.LabelCoverage)
	}
	if !ratioIs(got.ViaRouteShare, 3.0/5) { // earliest row: e1, e5, e6 via route; e2, e3 via status
		t.Errorf("via_route_share = %v, want 3/5", got.ViaRouteShare)
	}
	if got.DuplicateLabels != 1 {
		t.Errorf("duplicate_labels = %d, want 1", got.DuplicateLabels)
	}
	// Agent rows: e1, e2, e3 twice, e5, e6, e7's and the owner-only one.
	if got.LabelsTotal != 8 || got.OwnerOnlyLabels != 1 || got.NonRoutingLabels != 1 {
		t.Errorf("labels_total=%d owner_only=%d non_routing=%d, want 8, 1, 1", got.LabelsTotal, got.OwnerOnlyLabels, got.NonRoutingLabels)
	}
	if got.Overridden != 1 || !ratioIs(got.OverrideRate, 1.0/8) {
		t.Errorf("overridden=%d override_rate=%v, want 1 and 1/8", got.Overridden, got.OverrideRate)
	}
	if got.Window.Since == "" || got.Window.Until == "" {
		t.Errorf("window not echoed: %+v", got.Window)
	}

	// Per-class numbers cover the same window: the 8 agent rows and the human row.
	var decided, predicted, agreed int64
	for _, class := range got.Classes {
		decided, predicted, agreed = decided+class.Decided, predicted+class.Predicted, agreed+class.Agreed
	}
	if got.Decided != 9 || decided != 9 || predicted != 9 || agreed != 8 {
		t.Errorf("decided=%d (block %d) predicted=%d agreed=%d, want 9, 9, 9, 8", decided, got.Decided, predicted, agreed)
	}
}

// Spec decision 23: a label before `since` makes its in-window twin a duplicate.
func TestRouteReportDuplicateCountsLabelsBeforeTheWindow(t *testing.T) {
	s := newReportSeed(t)
	e := s.exit("2026-01-10T12:00:00+07:00", true)
	s.label(labelOpt{at: "2026-01-09T23:00:00+07:00", event: e, via: "status"}) // before since
	s.label(labelOpt{at: "2026-01-10T12:00:00+07:00", event: e})                // in window: the duplicate

	got := getRouteReport(t, windowQuery())
	if got.DuplicateLabels != 1 {
		t.Errorf("duplicate_labels = %d, want 1", got.DuplicateLabels)
	}
	if got.LabelsTotal != 1 {
		t.Errorf("labels_total = %d, want 1 (only the in-window row)", got.LabelsTotal)
	}
}

// With no window the report covers the last 24 hours.
func TestRouteReportDefaultWindowIsTheLast24Hours(t *testing.T) {
	s := newReportSeed(t)
	before := getRouteReport(t, "")
	stamp := func(ago time.Duration) string { return time.Now().Add(-ago).UTC().Format(time.RFC3339Nano) }

	inside := s.exit(stamp(time.Hour), true)
	s.label(labelOpt{at: stamp(time.Hour), event: inside})
	outside := s.exit(stamp(25*time.Hour), true)
	s.label(labelOpt{at: stamp(25 * time.Hour), event: outside})

	got := getRouteReport(t, "")
	if got.EligibleExits-before.EligibleExits != 1 || got.MatchedLabels-before.MatchedLabels != 1 || got.LabelsTotal-before.LabelsTotal != 1 {
		t.Errorf("last 24h delta: eligible=%d matched=%d labels=%d, want 1 each",
			got.EligibleExits-before.EligibleExits, got.MatchedLabels-before.MatchedLabels, got.LabelsTotal-before.LabelsTotal)
	}
	since, errSince := time.Parse(time.RFC3339, got.Window.Since)
	until, errUntil := time.Parse(time.RFC3339, got.Window.Until)
	if errSince != nil || errUntil != nil || until.Sub(since) != 24*time.Hour {
		t.Errorf("default window = %+v, want 24h", got.Window)
	}
}

func TestRouteReportRejectsABadWindow(t *testing.T) {
	for name, query := range map[string]string{
		"since not RFC 3339": "?since=yesterday",
		"until not RFC 3339": "?until=2026-01-10",
		"since after until":  "?since=" + url.QueryEscape(reportUntil) + "&until=" + url.QueryEscape(reportSince),
	} {
		t.Run(name, func(t *testing.T) {
			testutil.Call(t, testHandler.GetSystem1Report, newRequest(http.MethodGet, "/api/decisions/shadow-report"+query, nil)).Want(http.StatusBadRequest)
		})
	}
}

// A window with no events reports zeros, never NaN.
func TestRouteReportEmptyWindowIsZero(t *testing.T) {
	got := getRouteReport(t, "?since="+url.QueryEscape("2025-01-10T00:00:00+07:00")+"&until="+url.QueryEscape("2025-01-11T00:00:00+07:00"))
	if got.EligibleExits != 0 || got.MatchedLabels != 0 || got.LabelCoverage != 0 || got.ViaRouteShare != 0 || got.OverrideRate != 0 || got.LabelsTotal != 0 {
		t.Errorf("empty window = %+v", got)
	}
}
