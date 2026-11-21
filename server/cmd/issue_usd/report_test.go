package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func p64(v int64) *int64 { return &v }

func near(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

var wed = time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC) // Wed 10:00 ICT, week of Mon 16 Nov

// Prices used (USD per MTok, from internal/metrics/pricing.go):
//
//	claude-sonnet-5-5: in 2.00, read 0.10, write5m 2.50, write1h 4.00, out 10.00
//	claude-opus-5-5:   in 4.00, read 0.20, write5m 5.00, write1h 8.00, out 20.00
//
// Hand-computed run prices:
//
//	r1 sonnet in 1M, out 0.1M, read 2M, write 0.5M all 1h : 2.00+1.00+0.20+2.00      = 5.20
//	r2 opus   out 0.05M, read 1M, write 1M of which 0.4M 1h: 1.00+0.20+3.20+3.00      = 7.40
//	r3 sonnet in 0.5M, write 1M TTL unknown (5m rate)      : 1.00+2.50                 = 3.50
//	r4 sonnet out 0.2M                                     : 2.00
//	r5 sonnet out 0.1M                                     : 1.00
//	r6 sonnet out 0.1M + unpriced model (in 0.3M, out 0.1M): 1.00, 400000 uncosted tokens
//	r7 sonnet out 0.01M, no issue (chat run)               : 0.10
//	r8 running, no usage                                   : skipped
func fixture() ([]Run, map[string]Issue) {
	son := func(in, out, read, w int64, w1h *int64) Usage {
		return Usage{Provider: "claude", Model: "claude-sonnet-5-5", InputTokens: in, OutputTokens: out, CacheReadTokens: read, CacheWriteTokens: w, CacheWrite1hTokens: w1h}
	}
	runs := []Run{
		{ID: "r1", IssueID: "R1", CreatedAt: wed, Usage: []Usage{son(1_000_000, 100_000, 2_000_000, 500_000, p64(500_000))}},
		{ID: "r2", IssueID: "C1", CreatedAt: wed, Usage: []Usage{{Provider: "claude", Model: "claude-opus-5-5", OutputTokens: 50_000, CacheReadTokens: 1_000_000, CacheWriteTokens: 1_000_000, CacheWrite1hTokens: p64(400_000)}}},
		{ID: "r3", IssueID: "R1", CreatedAt: wed, Usage: []Usage{son(500_000, 0, 0, 1_000_000, nil)}},
		{ID: "r4", IssueID: "R3", CreatedAt: wed, Usage: []Usage{son(0, 200_000, 0, 0, p64(0))}},
		{ID: "r5", IssueID: "R2", CreatedAt: wed, Usage: []Usage{son(0, 100_000, 0, 0, nil)}},
		{ID: "r6", IssueID: "X", CreatedAt: wed, Usage: []Usage{son(0, 100_000, 0, 0, nil), {Provider: "mystery", Model: "mystery-model-1", InputTokens: 300_000, OutputTokens: 100_000}}},
		{ID: "r7", CreatedAt: wed, Usage: []Usage{son(0, 10_000, 0, 0, nil)}},
		{ID: "r8", IssueID: "R2", Status: "running", CreatedAt: wed},
	}
	issues := map[string]Issue{
		"R1": {ID: "R1", Identifier: "CHE-1", Status: "done", StatusCategory: "done"},
		"C1": {ID: "C1", Identifier: "CHE-2", ParentID: "R1", Status: "in_progress", StatusCategory: "in_progress"},
		"R2": {ID: "R2", Identifier: "CHE-3", Status: "in_progress", StatusCategory: "in_progress"},
		"R3": {ID: "R3", Identifier: "CHE-4", Status: "agent_accepted", StatusCategory: "done"},
		// "X" has a parent whose lookup failed: its root is unknown, not itself.
		"X": {ID: "X", Identifier: "CHE-5", ParentID: "gone", Status: "done", StatusCategory: "done"},
	}
	return runs, issues
}

func TestAggregateFixtureTotals(t *testing.T) {
	runs, issues := fixture()
	r := Aggregate(runs, issues)

	if r.RunCount != 7 || r.RunsWithoutUsage != 1 {
		t.Fatalf("runs = %d, without usage = %d; want 7 and 1", r.RunCount, r.RunsWithoutUsage)
	}
	// Sorted run prices: .10 1.00 1.00 2.00 3.50 5.20 7.40; nearest rank.
	if !near(r.RunP50, 2.00) || !near(r.RunP90, 7.40) {
		t.Fatalf("run p50 = %v, p90 = %v; want 2.00 and 7.40", r.RunP50, r.RunP90)
	}
	if !near(r.TotalUSD, 20.20) {
		t.Fatalf("total = %v, want 20.20", r.TotalUSD)
	}
	// Done roots: R1 = 5.20 + 7.40 (child C1) + 3.50 (rerun) = 16.10; R3 = 2.00.
	d := r.DoneRoots
	if d.Count != 2 || !near(d.TotalUSD, 18.10) || !near(d.MeanUSD, 9.05) || !near(d.P50, 2.00) || !near(d.P90, 16.10) {
		t.Fatalf("done roots = %+v; want n=2 total 18.10 mean 9.05 p50 2.00 p90 16.10", d)
	}
	if len(d.Top) != 2 || d.Top[0].Identifier != "CHE-1" || !near(d.Top[0].USD, 16.10) || d.Top[0].Runs != 3 {
		t.Fatalf("top = %+v; want CHE-1 16.10 over 3 runs first", d.Top)
	}
	// Not rolled into a done root: open root R2, unknown root (X), no issue.
	if !near(r.OpenRootsUSD, 1.00) || !near(r.UnknownRootUSD, 1.00) || !near(r.NoIssueUSD, 0.10) {
		t.Fatalf("open %v unknown-root %v no-issue %v; want 1.00 1.00 0.10", r.OpenRootsUSD, r.UnknownRootUSD, r.NoIssueUSD)
	}
	if r.UncostedTokens != 400_000 {
		t.Fatalf("uncosted = %d, want 400000", r.UncostedTokens)
	}
	if r.UncostedByModel["mystery-model-1"] != 400_000 || len(r.UncostedByModel) != 1 {
		t.Fatalf("uncosted by model = %v, want only mystery-model-1: 400000", r.UncostedByModel)
	}
	if out := Render(r); !strings.Contains(out, "mystery-model-1") {
		t.Fatalf("render misses the uncosted model:\n%s", out)
	}
	// TTL-capable writes: r1 0.5M (known), r2 1M (known), r3 1M (unknown), r4 0.
	if r.TTLUnknownTokens != 1_000_000 || r.TTLWriteTokens != 2_500_000 {
		t.Fatalf("ttl unknown %d of %d; want 1000000 of 2500000", r.TTLUnknownTokens, r.TTLWriteTokens)
	}
}

func TestTTLUnknownOverFivePercentIsAFinding(t *testing.T) {
	runs, issues := fixture()
	r := Aggregate(runs, issues)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %v, want exactly 1", r.Findings)
	}
	want := "FINDING: TTL unknown is 40.0% of cache-write tokens in the week of 2026-11-16 (limit 5%)"
	if r.Findings[0] != want {
		t.Fatalf("finding = %q, want %q", r.Findings[0], want)
	}
	if out := Render(r); !strings.Contains(out, want) {
		t.Fatalf("rendered report misses the finding line:\n%s", out)
	}
}

func TestTTLUnknownUnderFivePercentIsNoFinding(t *testing.T) {
	runs, issues := fixture()
	runs[2].Usage[0].CacheWrite1hTokens = p64(1_000_000) // r3 now known: 0% unknown
	r := Aggregate(runs, issues)
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %v, want none", r.Findings)
	}
}

func TestRootCycleDoesNotHang(t *testing.T) {
	issues := map[string]Issue{
		"A": {ID: "A", ParentID: "B", StatusCategory: "done"},
		"B": {ID: "B", ParentID: "A", StatusCategory: "done"},
	}
	runs := []Run{{ID: "r", IssueID: "A", CreatedAt: wed, Usage: []Usage{{Provider: "claude", Model: "claude-sonnet-5-5", OutputTokens: 100_000}}}}
	r := Aggregate(runs, issues)
	if !near(r.UnknownRootUSD, 1.00) {
		t.Fatalf("cycle run should be unknown-root 1.00, got %v", r.UnknownRootUSD)
	}
}

func TestFetchRunsPagesUntilCutoff(t *testing.T) {
	cutoff := wed
	pages := map[string]struct {
		runs []Run
		next string
	}{
		"":   {[]Run{{ID: "new1", CreatedAt: wed.Add(2 * time.Hour)}, {ID: "new2", CreatedAt: wed.Add(time.Hour)}}, "c1"},
		"c1": {[]Run{{ID: "edge", CreatedAt: wed}, {ID: "old", CreatedAt: wed.Add(-time.Hour)}}, "c2"},
		"c2": {[]Run{{ID: "never", CreatedAt: wed.Add(-48 * time.Hour)}}, ""},
	}
	var calls []string
	got, err := fetchRuns(func(before string) ([]Run, string, error) {
		calls = append(calls, before)
		p := pages[before]
		return p.runs, p.next, nil
	}, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if strings.Join(ids, ",") != "new1,new2,edge" {
		t.Fatalf("runs = %v, want new1,new2,edge", ids)
	}
	if strings.Join(calls, "|") != "|c1" {
		t.Fatalf("page calls = %q, want it to stop after the page that crossed the cutoff", calls)
	}
}

type fakeSource struct {
	pages     map[string]map[string]fakePage // agent -> cursor -> page
	issues    map[string]Issue
	failIssue map[string]bool
}

type fakePage struct {
	runs []Run
	next string
	err  error
}

func (f fakeSource) runsPage(agent, before string) ([]Run, string, error) {
	p := f.pages[agent][before]
	return p.runs, p.next, p.err
}

func (f fakeSource) issue(id string) (Issue, error) {
	if f.failIssue[id] {
		return Issue{}, errors.New("service unavailable")
	}
	return f.issues[id], nil
}

func sonnetRun(id, issue string) Run {
	return Run{ID: id, IssueID: issue, CreatedAt: wed, Usage: []Usage{{Provider: "claude", Model: "claude-sonnet-5-5", OutputTokens: 100_000}}}
}

func TestGatherCompleteHasNoFailures(t *testing.T) {
	src := fakeSource{
		pages:  map[string]map[string]fakePage{"a1": {"": {runs: []Run{sonnetRun("r1", "I1")}}}},
		issues: map[string]Issue{"I1": {ID: "I1", Identifier: "CHE-1", StatusCategory: "done"}},
	}
	g := gather(src, []string{"a1"}, wed.Add(-time.Hour), 2)
	if len(g.failures) != 0 || len(g.runs) != 1 || len(g.issues) != 1 {
		t.Fatalf("gather = %+v, want 1 run, 1 issue, no failures", g)
	}
}

// A page that fails after a good page drops that agent's earlier runs, so the
// totals are incomplete: it must be reported, not swallowed.
func TestGatherReportsPageFailureAfterGoodPage(t *testing.T) {
	src := fakeSource{
		pages: map[string]map[string]fakePage{"a1": {
			"":   {runs: []Run{sonnetRun("r1", "")}, next: "c1"},
			"c1": {err: errors.New("boom")},
		}},
	}
	g := gather(src, []string{"a1"}, wed.Add(-time.Hour), 2)
	if len(g.failures) != 1 {
		t.Fatalf("failures = %v, want exactly the page failure", g.failures)
	}
}

func TestGatherReportsFailedIssueLookup(t *testing.T) {
	src := fakeSource{
		pages:     map[string]map[string]fakePage{"a1": {"": {runs: []Run{sonnetRun("r1", "I1")}}}},
		failIssue: map[string]bool{"I1": true},
	}
	g := gather(src, []string{"a1"}, wed.Add(-time.Hour), 2)
	if len(g.failures) != 1 {
		t.Fatalf("failures = %v, want exactly the lookup failure", g.failures)
	}
	rep := Aggregate(g.runs, g.issues)
	if !near(rep.UnknownRootUSD, 1.00) {
		t.Fatalf("run with failed lookup should be unknown-root 1.00, got %v", rep.UnknownRootUSD)
	}
}

func TestReportFailedCallsIsExposedInJSONAndRender(t *testing.T) {
	rep := Aggregate(nil, nil)
	rep.FailedCalls = 3
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"failed_calls":3`) {
		t.Fatalf("json misses failed_calls: %s", b)
	}
	if out := Render(rep); !strings.Contains(out, "INCOMPLETE: 3 CLI calls failed") {
		t.Fatalf("render misses the incomplete line:\n%s", out)
	}
}

// execute is what main runs after listing agents; main exits 1 on its error.
func TestExecuteFailsOnFailedPageAfterGoodPage(t *testing.T) {
	src := fakeSource{pages: map[string]map[string]fakePage{"a1": {
		"":   {runs: []Run{sonnetRun("r1", "")}, next: "c1"},
		"c1": {err: errors.New("boom")},
	}}}
	var out bytes.Buffer
	err := execute(src, []string{"a1"}, wed.Add(-time.Hour), time.Time{}, true, 2, &out)
	if err == nil || !strings.Contains(err.Error(), "report is incomplete") {
		t.Fatalf("err = %v, want an incomplete-report error", err)
	}
	if !strings.Contains(out.String(), `"failed_calls": 1`) {
		t.Fatalf("json output misses failed_calls:\n%s", out.String())
	}
}

func TestExecuteFailsOnFailedIssueLookup(t *testing.T) {
	src := fakeSource{
		pages:     map[string]map[string]fakePage{"a1": {"": {runs: []Run{sonnetRun("r1", "I1")}}}},
		failIssue: map[string]bool{"I1": true},
	}
	var out bytes.Buffer
	err := execute(src, []string{"a1"}, wed.Add(-time.Hour), time.Time{}, false, 2, &out)
	if err == nil {
		t.Fatal("want an error for a failed issue lookup, got nil")
	}
	if !strings.Contains(out.String(), "INCOMPLETE: 1 CLI calls failed") {
		t.Fatalf("text output misses the INCOMPLETE line:\n%s", out.String())
	}
}

func TestExecuteSucceedsOnCompleteRead(t *testing.T) {
	src := fakeSource{
		pages:  map[string]map[string]fakePage{"a1": {"": {runs: []Run{sonnetRun("r1", "I1")}}}},
		issues: map[string]Issue{"I1": {ID: "I1", Identifier: "CHE-1", StatusCategory: "done"}},
	}
	var out bytes.Buffer
	if err := execute(src, []string{"a1"}, wed.Add(-time.Hour), time.Time{}, false, 2, &out); err != nil {
		t.Fatalf("complete read returned %v, want nil", err)
	}
	if strings.Contains(out.String(), "INCOMPLETE") || !strings.Contains(out.String(), "USD per done root issue (1 roots)") {
		t.Fatalf("unexpected output:\n%s", out.String())
	}
}

// CHE-6536: the daily check prices one ICT calendar day, not "the last 24 h",
// so it lines up with the sub2api day the watch reconciles against.
func TestDayWindowIsOneICTCalendarDay(t *testing.T) {
	start, end, err := dayWindow("2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Fatalf("start = %s, want %s (00:00 ICT)", start.UTC(), want)
	}
	if want := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC); !end.Equal(want) {
		t.Fatalf("end = %s, want %s (24 h later)", end.UTC(), want)
	}
	if _, _, err := dayWindow("20-11-2026"); err == nil {
		t.Fatal("a malformed day must be an error")
	}
}

func TestExecuteDropsRunsAtOrAfterEnd(t *testing.T) {
	in := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)    // 10:00 ICT on 20 Nov
	next := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC) // 00:00 ICT on 21 Nov: next day
	r1, r2 := sonnetRun("r-in", ""), sonnetRun("r-next", "")
	r1.CreatedAt, r2.CreatedAt = in, next
	src := fakeSource{pages: map[string]map[string]fakePage{"a1": {"": {runs: []Run{r2, r1}}}}}
	var out bytes.Buffer
	start := in.Add(-time.Hour)
	if err := execute(src, []string{"a1"}, start, next, true, 2, &out); err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.RunCount != 1 {
		t.Fatalf("runs_priced = %d, want 1 (the run at the end bound is the next day)", rep.RunCount)
	}
}
