package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var jevAskedAt = time.Date(2026, 11, 2, 10, 0, 0, 0, ictZone)

// n decided rows; the first `agreed` match Cheese's answer. Each was decided one minute after the ask.
func jevFixtureRows(n, agreed int) ([]jevShadowRow, []jevAsk) {
	var rows []jevShadowRow
	var asks []jevAsk
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ask-%d", i)
		pick, answer := "Proceed", "Proceed"
		if i >= agreed {
			answer = "Hold"
		}
		rows = append(rows, jevShadowRow{At: jevAskedAt.Add(time.Minute).Format(time.RFC3339), ID: id, Class: "reversible", Route: "jev", Reason: "decided", Pick: pick})
		asks = append(asks, jevAsk{ID: id, CreatedAt: jevAskedAt.UTC().Format(time.RFC3339), Answer: answer})
	}
	return rows, asks
}

func TestJevShadowStatsZeroData(t *testing.T) {
	s := jevShadowStats(nil, nil)
	if s.Asks != 0 || s.Agreed != 0 || s.Rate != 0 || s.InScope != 0 || s.Within10 != 0 || s.Within10Rate != 0 || s.GateHolds {
		t.Fatalf("zero data: %+v, want all zero and the gate not holding", s)
	}
}

func TestJevShadowStatsLiveGate(t *testing.T) {
	cases := []struct {
		name          string
		n, agreed     int
		wantHold      bool
		wantRate      float64
		wantAskCounts int
	}{
		{"90% on 20 holds", 20, 18, true, 0.9, 20},
		{"all agree on 20 holds", 20, 20, true, 1, 20},
		{"85% on 20 does not hold", 20, 17, false, 0.85, 20},
		{"100% on 19 does not hold", 19, 19, false, 1, 19},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, asks := jevFixtureRows(tc.n, tc.agreed)
			s := jevShadowStats(rows, asks)
			if s.GateHolds != tc.wantHold || s.Rate != tc.wantRate || s.Asks != tc.wantAskCounts || s.Agreed != tc.agreed {
				t.Fatalf("%+v, want hold=%v rate=%v asks=%d agreed=%d", s, tc.wantHold, tc.wantRate, tc.wantAskCounts, tc.agreed)
			}
		})
	}
}

// Only asks Cheese has answered are graded; Cheese-routed rows never are.
func TestJevShadowStatsGradesAnsweredJevRowsOnly(t *testing.T) {
	rows, asks := jevFixtureRows(3, 3)
	asks[2].Answer = ""
	rows = append(rows, jevShadowRow{At: jevAskedAt.Format(time.RFC3339), ID: "cheese-1", Class: "irreversible", Route: "cheese", Reason: "excluded"})
	asks = append(asks, jevAsk{ID: "cheese-1", CreatedAt: jevAskedAt.UTC().Format(time.RFC3339), Answer: "Proceed"})
	s := jevShadowStats(rows, asks)
	if s.Asks != 2 || s.Agreed != 2 {
		t.Fatalf("graded %d agreed %d, want 2 and 2: %+v", s.Asks, s.Agreed, s)
	}
}

// In scope = every logged ask that is not an excluded class. Decided in time = Jev picked within 10 minutes of the ask.
func TestJevShadowStatsTenMinuteShare(t *testing.T) {
	at := func(d time.Duration) string { return jevAskedAt.Add(d).Format(time.RFC3339) }
	created := jevAskedAt.UTC().Format(time.RFC3339)
	rows := []jevShadowRow{
		{At: at(9 * time.Minute), ID: "a", Class: "reversible", Route: "jev", Reason: "decided", Pick: "Proceed"},
		{At: at(10 * time.Minute), ID: "b", Class: "reversible", Route: "jev", Reason: "decided", Pick: "Proceed"},
		{At: at(11 * time.Minute), ID: "c", Class: "spend", Route: "jev", Reason: "decided", Pick: "Proceed"},
		{At: at(time.Minute), ID: "d", Class: "deploy", Route: "cheese", Reason: "unsure"},
		{At: at(time.Minute), ID: "e", Class: "irreversible", Route: "cheese", Reason: "excluded"},
		{At: at(time.Minute), ID: "missing", Class: "reversible", Route: "jev", Reason: "decided", Pick: "Proceed"},
	}
	asks := []jevAsk{{ID: "a", CreatedAt: created}, {ID: "b", CreatedAt: created}, {ID: "c", CreatedAt: created}, {ID: "d", CreatedAt: created}, {ID: "e", CreatedAt: created}}
	s := jevShadowStats(rows, asks)
	if s.InScope != 5 || s.Within10 != 2 || s.Within10Rate != 0.4 {
		t.Fatalf("in scope %d, within 10 min %d, rate %v; want 5, 2, 0.4", s.InScope, s.Within10, s.Within10Rate)
	}
}

func TestReadJevShadowLog(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "shadow.jsonl")
	line := `{"at":"2026-11-02T10:01:00+07:00","id":"x","class":"reversible","route":"jev","reason":"decided","pick":"Hold","confidence":0.9}`
	if err := os.WriteFile(good, []byte(line+"\n\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := readJevShadowLog(good)
	if err != nil || len(rows) != 2 || rows[0].Pick != "Hold" || rows[0].Route != "jev" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJevShadowLog(bad); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("bad log: err=%v, want a line-numbered error", err)
	}
	if _, err := readJevShadowLog(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Fatal("missing log must be an error")
	}
}

func jevFeed() map[string]any {
	return map[string]any{
		"enabled": true, "decisions": []any{},
		"waivers": []any{
			map[string]any{"agent_id": "a1", "agent": "x99-claude-sonnet-200k", "count": 3},
			map[string]any{"agent_id": "a2", "agent": "x99-codex-luna-2", "count": 1},
		},
		"jev": map[string]any{"decisions": 4, "vetoed": 1, "veto_rate": 0.25, "excluded_class_decisions": 0},
	}
}

func TestAgentDecisionMarkdownShowsWaiversAndJevMetrics(t *testing.T) {
	rows, asks := jevFixtureRows(20, 18)
	feed := jevFeed()
	feed["jev_asks"] = asks
	var b strings.Builder
	if err := writeAgentDecisionMarkdown(&b, feed, &jevShadowLog{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Waivers (WAIVED, not PASS)", "x99-claude-sonnet-200k: 3", "x99-codex-luna-2: 1",
		"Jev: 4 decisions", "veto rate 25%", "excluded-class decisions: 0",
		"Shadow agreement with Cheese: 90% (18 of 20 asks)", "decided within 10 minutes: 100% (20 of 20 in-scope asks)",
		"Live-mode gate: HOLDS",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, b.String())
		}
	}
}

func TestAgentDecisionMarkdownGateDoesNotHold(t *testing.T) {
	rows, asks := jevFixtureRows(12, 12)
	feed := jevFeed()
	feed["jev_asks"] = asks
	var b strings.Builder
	if err := writeAgentDecisionMarkdown(&b, feed, &jevShadowLog{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Live-mode gate: NOT MET (needs 90% on at least 20 asks; now 100% on 12)") {
		t.Errorf("gate line missing:\n%s", b.String())
	}
}

func TestAgentDecisionMarkdownWithNoJevData(t *testing.T) {
	feed := map[string]any{"enabled": true, "decisions": []any{}, "waivers": []any{}, "jev": map[string]any{"decisions": 0, "vetoed": 0, "veto_rate": 0, "excluded_class_decisions": 0}}
	var b strings.Builder
	if err := writeAgentDecisionMarkdown(&b, feed, &jevShadowLog{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Waivers (WAIVED, not PASS): none", "Jev: 0 decisions", "veto rate 0%", "excluded-class decisions: 0",
		"Shadow agreement with Cheese: no graded asks", "Live-mode gate: NOT MET (needs 90% on at least 20 asks; now 0% on 0)"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, b.String())
		}
	}
}

func TestAgentDecisionMarkdownFlagsExcludedClassDecisions(t *testing.T) {
	feed := jevFeed()
	feed["jev"] = map[string]any{"decisions": 4, "vetoed": 0, "veto_rate": 0, "excluded_class_decisions": 2}
	var b strings.Builder
	if err := writeAgentDecisionMarkdown(&b, feed, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "excluded-class decisions: 2 (must be 0)") {
		t.Errorf("non-zero excluded count not flagged:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "Shadow log not given") {
		t.Errorf("missing log not stated:\n%s", b.String())
	}
}

// The command reads the shadow log, asks the server about the Jev-decided ids and prints the verdict.
func TestAgentDecisionBriefCommandUsesShadowLog(t *testing.T) {
	rows, asks := jevFixtureRows(20, 19)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "shadow.jsonl")
	var lines []string
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		lines = append(lines, string(raw))
	}
	lines = append(lines, `{"at":"2026-11-02T10:00:00+07:00","id":"cheese-1","class":"irreversible","route":"cheese","reason":"excluded"}`)
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIDs = r.URL.Query()["jev_ask"]
		feed := jevFeed()
		feed["jev_asks"] = asks
		json.NewEncoder(w).Encode(feed)
	}))
	defer server.Close()
	t.Setenv("MULTICA_SERVER_URL", server.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	brief, _, _ := rootCmd.Find([]string{"decision", "brief"})
	for k, v := range map[string]string{"format": "markdown", "jev-shadow-log": logPath} {
		if err := brief.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	defer brief.Flags().Set("format", "json")
	defer brief.Flags().Set("jev-shadow-log", "")
	out, err := captureStdout(t, func() error { return brief.RunE(brief, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if len(gotIDs) != 20 {
		t.Fatalf("server asked about %d ids, want the 20 Jev-decided ones (not the Cheese-routed one): %v", len(gotIDs), gotIDs)
	}
	if !strings.Contains(out, "Shadow agreement with Cheese: 95% (19 of 20 asks)") || !strings.Contains(out, "Live-mode gate: HOLDS") {
		t.Errorf("output:\n%s", out)
	}
}
