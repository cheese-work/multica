package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/spf13/cobra"
)

// The daily brief autopilot prints its "Agents decided — veto?" section from
// `multica decision brief`. The owner flips the pilot with `agent-setting`.
func init() {
	brief := &cobra.Command{Use: "brief", Short: "Read agent-made decisions for the daily brief: records, delivery failures and pilot metrics", Args: cobra.NoArgs, RunE: runAgentDecisionBrief}
	brief.Flags().String("format", "json", "json, or markdown for the brief section \"Agents decided — veto?\"")
	brief.Flags().String("jev-shadow-log", "", "path to the Jev shadow log (JSONL); default $JEV_STATE_DIR/shadow.jsonl, the Jev decider's own log")
	setting := &cobra.Command{Use: "agent-setting", Short: "Turn agent decisions on or off for the workspace (owner only; default off)", Args: cobra.NoArgs, RunE: runAgentDecisionSetting}
	setting.Flags().Bool("enabled", false, "true turns agent decisions on, false turns them off")
	_ = setting.MarkFlagRequired("enabled")
	decisionCmd.AddCommand(brief, setting)
}

func runAgentDecisionBrief(command *cobra.Command, _ []string) error {
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	logPath, _ := command.Flags().GetString("jev-shadow-log")
	// A named log must exist. The default log may be missing: the brief then says so.
	explicit := logPath != ""
	if !explicit {
		logPath = defaultJevShadowLog()
	}
	shadow := &jevShadowLog{}
	path := "/api/agent-decisions/brief"
	if _, statErr := os.Stat(logPath); explicit || statErr == nil {
		rows, err := readJevShadowLog(logPath)
		if err != nil {
			return err
		}
		shadow.Rows = rows
		ids := shadow.jevIDs()
		if len(ids) > jevMaxAsks {
			return fmt.Errorf("shadow log has %d Jev asks, the brief reads at most %d: pass a shorter log", len(ids), jevMaxAsks)
		}
		q := url.Values{"jev_ask": ids}
		if since, ok := shadow.since(); ok {
			q.Set("since", since.UTC().Format(time.RFC3339))
		}
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
	} else {
		shadow.Missing = logPath
	}
	var out map[string]any
	if err := client.GetJSON(ctx, path, &out); err != nil {
		return err
	}
	format, _ := command.Flags().GetString("format")
	switch format {
	case "json":
		if shadow.Missing == "" {
			out["jev_shadow"] = jevShadowStats(shadow.Rows, jevAsksOf(out), jevWindowOf(out))
		}
		return cli.PrintJSON(os.Stdout, out)
	case "markdown":
		return writeAgentDecisionMarkdown(os.Stdout, out, shadow)
	}
	return fmt.Errorf("unknown --format %q: use json or markdown", format)
}

var ictZone = time.FixedZone("ICT", 7*60*60)

// The section stands on its own: the daily brief pastes it after Needs you,
// never inside it. Times are ICT.
func writeAgentDecisionMarkdown(w io.Writer, feed map[string]any, shadow *jevShadowLog) error {
	records, _ := feed["decisions"].([]any)
	failures, _ := feed["notice_failures"].([]any)
	askFailures, _ := feed["ask_failures"].([]any)
	stopped, isStopped := feed["stopped"].(map[string]any)
	// With the pilot off the brief stays as it was: no section. Records made
	// earlier stay vetoable, so they and a stop or failed notice still print.
	waivers, _ := feed["waivers"].([]any)
	jev, _ := feed["jev"].(map[string]any)
	if enabled, _ := feed["enabled"].(bool); !enabled && len(records) == 0 && len(failures) == 0 && len(askFailures) == 0 && !isStopped && len(waivers) == 0 && jevNumber(jev["decisions"]) == 0 && (shadow == nil || shadow.Missing != "") {
		return nil
	}
	var b strings.Builder
	b.WriteString("## Agents decided — veto?\n\n")
	if isStopped {
		fmt.Fprintf(&b, "Agent decisions are stopped: %v (%s).\n\n", stopped["reason"], ictTime(stopped["at"]))
	}
	if len(records) == 0 {
		b.WriteString("No agent decisions to review.\n")
	}
	for _, raw := range records {
		rec, _ := raw.(map[string]any)
		fmt.Fprintf(&b, "- %v · %v · %v", rec["identifier"], rec["gate"], rec["state"])
		if outcome, _ := rec["outcome"].(string); outcome != "" {
			fmt.Fprintf(&b, " · %s", outcome)
		}
		fmt.Fprintf(&b, "\n  Record: %v\n", rec["link"])
		if decider, _ := rec["decider"].(string); decider != "" {
			// An older server sends no decider_display: print the raw value.
			if display, _ := rec["decider_display"].(string); display != "" {
				decider = display
			}
			fmt.Fprintf(&b, "  Decided by %s (agent decision, confidence %v).\n", decider, rec["confidence"])
		}
		if rec["state"] == "revoked" {
			fmt.Fprintf(&b, "  Revoked: %v\n", rec["revoke_reason"])
			continue
		}
		fmt.Fprintf(&b, "  Review by %s (a target, not an expiry).\n  To veto, comment on the issue:\n  `%v` followed by your reason.\n", ictTime(rec["review_target_at"]), rec["veto_line"])
	}
	if len(failures) > 0 {
		b.WriteString("\nNotice delivery failed:\n")
		for _, raw := range failures {
			f, _ := raw.(map[string]any)
			fmt.Fprintf(&b, "- decision %v, %v attempts: %v\n", f["instance_id"], f["attempts"], f["last_error"])
		}
	}
	if len(askFailures) > 0 {
		b.WriteString("\nAsks that never reached a person:\n")
		for _, raw := range askFailures {
			f, _ := raw.(map[string]any)
			fmt.Fprintf(&b, "- %v · owner %v · %v · %s\n", f["identifier"], f["agent"], f["reason"], ictTime(f["failed_at"]))
			if excerpt, _ := f["excerpt"].(string); excerpt != "" {
				fmt.Fprintf(&b, "  Ask: %s\n", excerpt)
			}
		}
	}
	writeJevMetrics(&b, waivers, jev, shadow, jevAsksOf(feed), jevWindowOf(feed))
	_, err := io.WriteString(w, b.String())
	return err
}

func ictTime(value any) string {
	text, _ := value.(string)
	t, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return text
	}
	return t.In(ictZone).Format("2006-01-02 15:04 ICT")
}

func runAgentDecisionSetting(command *cobra.Command, _ []string) error {
	enabled, _ := command.Flags().GetBool("enabled")
	client, err := newAPIClient(command)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.PutJSON(ctx, "/api/agent-decisions/setting", map[string]bool{"enabled": enabled}, &out); err != nil {
		return err
	}
	return cli.PrintJSON(os.Stdout, out)
}

const (
	jevMaxAsks      = 1000
	jevGateMinAsks  = 20
	jevGateAgreePct = 90
	jevDecideWithin = 10 * time.Minute
)

// A row of the Jev shadow log (scripts/jev_decider.py). Route "jev" means Jev
// picked; "cheese" means the ask stayed with Cheese. Reason "excluded" is a
// class that never goes to Jev.
type jevShadowRow struct {
	At     string `json:"at"`
	ID     string `json:"id"`
	Class  string `json:"class"`
	Route  string `json:"route"`
	Reason string `json:"reason"`
	Pick   string `json:"pick"`
}

// What the server knows about one logged ask: when it was asked, what Cheese chose.
type jevAsk struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Answer    string `json:"answer"`
}

type jevShadowLog struct {
	Rows []jevShadowRow
	// Set when the default log does not exist.
	Missing string
}

// The decider's own default (scripts/jev_decider.py): $JEV_STATE_DIR/shadow.jsonl.
func defaultJevShadowLog() string {
	dir := os.Getenv("JEV_STATE_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state", "jev-decider")
	}
	return filepath.Join(dir, "shadow.jsonl")
}

// When the log began: the earliest row.
func (l *jevShadowLog) since() (time.Time, bool) {
	var first time.Time
	for _, r := range l.Rows {
		if t, err := time.Parse(time.RFC3339, r.At); err == nil && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first, !first.IsZero()
}

func (l *jevShadowLog) jevIDs() []string {
	var ids []string
	seen := map[string]bool{}
	for _, r := range l.Rows {
		if r.Route == "jev" && r.ID != "" && !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	return ids
}

type jevStats struct {
	// Asks Jev picked for and Cheese answered; Agreed of them match.
	Asks, Agreed int
	Rate         float64
	// In-scope asks are every logged ask outside an excluded class.
	InScope, Within10 int
	Within10Rate      float64
	// Live mode needs 90% agreement on at least 20 asks.
	GateHolds bool
}

// What the server counted in the log's window; nil means the log alone.
type jevWindow struct {
	Since   time.Time
	InScope int
}

func jevShadowStats(rows []jevShadowRow, asks []jevAsk, w *jevWindow) jevStats {
	byID := map[string]jevAsk{}
	for _, a := range asks {
		byID[a.ID] = a
	}
	// One ask can be logged more than once; the last row stands.
	last := map[string]jevShadowRow{}
	var order []string
	for _, r := range rows {
		if _, ok := last[r.ID]; !ok {
			order = append(order, r.ID)
		}
		last[r.ID] = r
	}
	var s jevStats
	for _, id := range order {
		r, ask := last[id], byID[id]
		if r.Reason == "excluded" {
			continue
		}
		s.InScope++
		if r.Route != "jev" {
			continue
		}
		if ask.Answer != "" {
			s.Asks++
			if r.Pick == ask.Answer {
				s.Agreed++
			}
		}
		decided, err1 := time.Parse(time.RFC3339, r.At)
		asked, err2 := time.Parse(time.RFC3339, ask.CreatedAt)
		if err1 == nil && err2 == nil && decided.Sub(asked) <= jevDecideWithin {
			s.Within10++
		}
	}
	if w != nil {
		s.InScope = w.InScope // includes budget and API-error attempts, which the log never holds
	}
	if s.Asks > 0 {
		s.Rate = float64(s.Agreed) / float64(s.Asks)
	}
	if s.InScope > 0 {
		s.Within10Rate = float64(s.Within10) / float64(s.InScope)
	}
	s.GateHolds = s.Asks >= jevGateMinAsks && s.Agreed*100 >= s.Asks*jevGateAgreePct
	return s
}

func readJevShadowLog(path string) ([]jevShadowRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []jevShadowRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r jevShadowRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		rows = append(rows, r)
	}
	return rows, sc.Err()
}

// The feed holds jev_asks as plain JSON; tests hand in typed values, so go through JSON.
func jevAsksOf(feed map[string]any) []jevAsk {
	raw, err := json.Marshal(feed["jev_asks"])
	if err != nil {
		return nil
	}
	var asks []jevAsk
	_ = json.Unmarshal(raw, &asks)
	return asks
}

func jevNumber(v any) float64 {
	n, _ := v.(float64)
	if n == 0 {
		if i, ok := v.(int); ok {
			return float64(i)
		}
	}
	return n
}

// The server's count of in-scope asks since the log began; nil when the feed has none.
func jevWindowOf(feed map[string]any) *jevWindow {
	n, ok := feed["jev_in_scope"]
	since, err := time.Parse(time.RFC3339, fmt.Sprint(feed["jev_since"]))
	if !ok || err != nil {
		return nil
	}
	return &jevWindow{Since: since, InScope: int(jevNumber(n))}
}

func percent(rate float64) string { return fmt.Sprintf("%.0f%%", rate*100) }

func writeJevMetrics(b *strings.Builder, waivers []any, jev map[string]any, shadow *jevShadowLog, asks []jevAsk, w *jevWindow) {
	if len(waivers) == 0 {
		b.WriteString("\nWaivers (WAIVED, not PASS): none\n")
	} else {
		b.WriteString("\nWaivers (WAIVED, not PASS):\n")
		for _, raw := range waivers {
			w, _ := raw.(map[string]any)
			fmt.Fprintf(b, "- %v: %.0f\n", w["agent"], jevNumber(w["count"]))
		}
	}
	excluded := jevNumber(jev["excluded_class_decisions"])
	flag := ""
	if excluded != 0 {
		flag = " (must be 0)"
	}
	fmt.Fprintf(b, "Jev: %.0f decisions · veto rate %s (%.0f vetoed) · excluded-class decisions: %.0f%s\n",
		jevNumber(jev["decisions"]), percent(jevNumber(jev["veto_rate"])), jevNumber(jev["vetoed"]), excluded, flag)
	if shadow == nil {
		b.WriteString("Shadow log not given: agreement, 10-minute share and live-mode gate not shown.\n")
		return
	}
	s := jevShadowStats(shadow.Rows, asks, w)
	if shadow.Missing != "" {
		fmt.Fprintf(b, "Shadow log not found at %s: agreement and 10-minute share not shown.\n", shadow.Missing)
		fmt.Fprintf(b, "Live-mode gate: NOT MET (needs %d%% on at least %d asks; no log)\n", jevGateAgreePct, jevGateMinAsks)
		return
	}
	if s.Asks == 0 {
		b.WriteString("Shadow agreement with Cheese: no graded asks\n")
	} else {
		fmt.Fprintf(b, "Shadow agreement with Cheese: %s (%d of %d asks)\n", percent(s.Rate), s.Agreed, s.Asks)
	}
	if s.InScope == 0 {
		b.WriteString("Jev decided within 10 minutes: no in-scope asks\n")
	} else {
		fmt.Fprintf(b, "Jev decided within 10 minutes: %s (%d of %d in-scope asks)\n", percent(s.Within10Rate), s.Within10, s.InScope)
	}
	if s.GateHolds {
		b.WriteString("Live-mode gate: HOLDS\n")
	} else {
		fmt.Fprintf(b, "Live-mode gate: NOT MET (needs %d%% on at least %d asks; now %s on %d)\n", jevGateAgreePct, jevGateMinAsks, percent(s.Rate), s.Asks)
	}
}
