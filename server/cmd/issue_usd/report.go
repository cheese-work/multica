package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/metrics"
)

// ttlUnknownLimit is the share of 1-hour-capable cache-write tokens without a
// TTL split above which a week is reported as a finding (plan CHE-6498 section 6).
const ttlUnknownLimit = 0.05

// maxChain bounds the parent walk so a cyclic parent chain cannot hang.
const maxChain = 32

// ict is the zone weeks are cut in: a week starts Monday 00:00 ICT.
var ict = time.FixedZone("ICT", 7*3600)

// Usage is one (provider, model) slice of a run's token usage, as printed by
// `multica agent tasks <id> --output json` in the run's `usage` array.
type Usage struct {
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	InputTokens        int64  `json:"input_tokens"`
	OutputTokens       int64  `json:"output_tokens"`
	CacheReadTokens    int64  `json:"cache_read_tokens"`
	CacheWriteTokens   int64  `json:"cache_write_tokens"`
	CacheWrite1hTokens *int64 `json:"cache_write_1h_tokens"`
}

func (u Usage) totalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Run is one agent task.
type Run struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	IssueID   string    `json:"issue_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	Usage     []Usage   `json:"usage"`
}

// Issue is the part of `multica issue get` that rolls a run up to its root.
type Issue struct {
	ID             string `json:"id"`
	Identifier     string `json:"identifier"`
	ParentID       string `json:"parent_issue_id"`
	Status         string `json:"status"`
	StatusCategory string `json:"status_category"`
}

func (i Issue) isDone() bool {
	if i.StatusCategory != "" {
		return i.StatusCategory == "done"
	}
	return i.Status == "done" || i.Status == "agent_accepted"
}

type RootCost struct {
	Identifier string  `json:"identifier"`
	USD        float64 `json:"usd"`
	Runs       int     `json:"runs"`
}

type RootStats struct {
	Count    int        `json:"count"`
	TotalUSD float64    `json:"total_usd"`
	MeanUSD  float64    `json:"mean_usd"`
	P50      float64    `json:"p50_usd"`
	P90      float64    `json:"p90_usd"`
	Top      []RootCost `json:"top"`
}

type Report struct {
	RunCount         int       `json:"runs_priced"`
	RunsWithoutUsage int       `json:"runs_without_usage"`
	TotalUSD         float64   `json:"total_usd"`
	RunP50           float64   `json:"run_p50_usd"`
	RunP90           float64   `json:"run_p90_usd"`
	DoneRoots        RootStats `json:"done_roots"`
	OpenRootsUSD     float64   `json:"open_roots_usd"`
	UnknownRootUSD   float64   `json:"unknown_root_usd"`
	NoIssueUSD       float64   `json:"no_issue_usd"`
	UncostedTokens   int64     `json:"uncosted_tokens"`
	// UncostedByModel splits UncostedTokens by the model id that had no price.
	UncostedByModel map[string]int64 `json:"uncosted_by_model"`
	// TTLWriteTokens counts cache-write tokens of SKUs that have a 1-hour rate;
	// TTLUnknownTokens is the part of those with no reported TTL split.
	TTLWriteTokens   int64    `json:"ttl_write_tokens"`
	TTLUnknownTokens int64    `json:"ttl_unknown_tokens"`
	Findings         []string `json:"findings"`
	// FailedCalls counts CLI reads that failed after retries; non-zero means
	// the totals are incomplete.
	FailedCalls int `json:"failed_calls"`
}

type weekTTL struct{ unknown, total int64 }

// runUSD prices one run with the shared price table. A usage row whose model
// has no price adds its tokens to `uncosted` and nothing to USD. priced is
// false when no row could be priced.
func runUSD(r Run, week *weekTTL, rep *Report) (usd float64, priced bool) {
	for _, u := range r.Usage {
		price, ok := metrics.PriceForModelAlias(u.Model)
		if !ok {
			rep.UncostedTokens += u.totalTokens()
			if rep.UncostedByModel == nil {
				rep.UncostedByModel = map[string]int64{}
			}
			rep.UncostedByModel[u.Model] += u.totalTokens()
			continue
		}
		priced = true
		usd += price.EstimateUSD(u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CacheWrite1hTokens)
		if price.CacheWrite1hPerM > 0 && u.CacheWriteTokens > 0 {
			week.total += u.CacheWriteTokens
			if u.CacheWrite1hTokens == nil {
				week.unknown += u.CacheWriteTokens
			}
		}
	}
	return usd, priced
}

// rootOf walks parent links to the root. ok is false when any link is missing
// from `issues` (lookup failed) or the chain cycles: the root is then unknown,
// never the issue itself.
func rootOf(id string, issues map[string]Issue) (Issue, bool) {
	for range maxChain {
		cur, found := issues[id]
		if !found {
			return Issue{}, false
		}
		if cur.ParentID == "" {
			return cur, true
		}
		id = cur.ParentID
	}
	return Issue{}, false
}

// Aggregate prices every run, rolls children and reruns up to the root issue
// and summarises. `runs` must already be cut to the window.
func Aggregate(runs []Run, issues map[string]Issue) Report {
	var rep Report
	var runUSDs []float64
	weeks := map[string]*weekTTL{}
	type rootAcc struct {
		root Issue
		usd  float64
		runs int
	}
	roots := map[string]*rootAcc{}

	for _, r := range runs {
		if len(r.Usage) == 0 {
			rep.RunsWithoutUsage++
			continue
		}
		key := r.CreatedAt.In(ict)
		key = time.Date(key.Year(), key.Month(), key.Day()-(int(key.Weekday())+6)%7, 0, 0, 0, 0, ict)
		wk := key.Format("2006-01-02")
		if weeks[wk] == nil {
			weeks[wk] = &weekTTL{}
		}
		usd, priced := runUSD(r, weeks[wk], &rep)
		if !priced {
			continue
		}
		rep.RunCount++
		rep.TotalUSD += usd
		runUSDs = append(runUSDs, usd)

		if r.IssueID == "" {
			rep.NoIssueUSD += usd
			continue
		}
		root, ok := rootOf(r.IssueID, issues)
		if !ok {
			rep.UnknownRootUSD += usd
			continue
		}
		// Key by identifier when present: one issue can be reached by UUID
		// and by identifier, and must stay one root.
		rootKey := root.Identifier
		if rootKey == "" {
			rootKey = root.ID
		}
		acc := roots[rootKey]
		if acc == nil {
			acc = &rootAcc{root: root}
			roots[rootKey] = acc
		}
		acc.usd += usd
		acc.runs++
	}

	var doneUSDs []float64
	for _, acc := range roots {
		if !acc.root.isDone() {
			rep.OpenRootsUSD += acc.usd
			continue
		}
		doneUSDs = append(doneUSDs, acc.usd)
		rep.DoneRoots.TotalUSD += acc.usd
		rep.DoneRoots.Top = append(rep.DoneRoots.Top, RootCost{Identifier: acc.root.Identifier, USD: acc.usd, Runs: acc.runs})
	}
	rep.DoneRoots.Count = len(doneUSDs)
	if n := len(doneUSDs); n > 0 {
		rep.DoneRoots.MeanUSD = rep.DoneRoots.TotalUSD / float64(n)
		rep.DoneRoots.P50 = percentile(doneUSDs, 0.50)
		rep.DoneRoots.P90 = percentile(doneUSDs, 0.90)
	}
	sort.Slice(rep.DoneRoots.Top, func(i, j int) bool {
		a, b := rep.DoneRoots.Top[i], rep.DoneRoots.Top[j]
		if a.USD != b.USD {
			return a.USD > b.USD
		}
		return a.Identifier < b.Identifier
	})
	if len(rep.DoneRoots.Top) > 10 {
		rep.DoneRoots.Top = rep.DoneRoots.Top[:10]
	}
	rep.RunP50 = percentile(runUSDs, 0.50)
	rep.RunP90 = percentile(runUSDs, 0.90)

	var weekKeys []string
	for k, w := range weeks {
		rep.TTLWriteTokens += w.total
		rep.TTLUnknownTokens += w.unknown
		weekKeys = append(weekKeys, k)
	}
	sort.Strings(weekKeys)
	for _, k := range weekKeys {
		w := weeks[k]
		if w.total > 0 && float64(w.unknown)/float64(w.total) > ttlUnknownLimit {
			rep.Findings = append(rep.Findings, fmt.Sprintf(
				"FINDING: TTL unknown is %.1f%% of cache-write tokens in the week of %s (limit %.0f%%)",
				100*float64(w.unknown)/float64(w.total), k, 100*ttlUnknownLimit))
		}
	}
	return rep
}

// percentile is nearest-rank: the smallest value with at least p of the sample
// at or below it. It returns 0 for an empty sample.
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	rank := int(math.Ceil(p*float64(len(s)) - 1e-9))
	return s[min(max(rank, 1), len(s))-1]
}

// Render is the plain-text report.
func Render(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "USD per run (%d runs priced, %d without usage): total $%.2f, p50 $%.2f, p90 $%.2f\n",
		r.RunCount, r.RunsWithoutUsage, r.TotalUSD, r.RunP50, r.RunP90)
	d := r.DoneRoots
	fmt.Fprintf(&b, "USD per done root issue (%d roots): total $%.2f, mean $%.2f, p50 $%.2f, p90 $%.2f\n",
		d.Count, d.TotalUSD, d.MeanUSD, d.P50, d.P90)
	for _, t := range d.Top {
		fmt.Fprintf(&b, "  %-12s $%9.2f  %d runs\n", t.Identifier, t.USD, t.Runs)
	}
	fmt.Fprintf(&b, "Not in a done root: open roots $%.2f, unknown root $%.2f, runs without an issue $%.2f\n",
		r.OpenRootsUSD, r.UnknownRootUSD, r.NoIssueUSD)
	fmt.Fprintf(&b, "Uncosted tokens (model has no price): %d\n", r.UncostedTokens)
	models := make([]string, 0, len(r.UncostedByModel))
	for m := range r.UncostedByModel {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		a, b := r.UncostedByModel[models[i]], r.UncostedByModel[models[j]]
		if a != b {
			return a > b
		}
		return models[i] < models[j]
	})
	for i, m := range models {
		if i == 5 {
			fmt.Fprintf(&b, "  ... and %d more models\n", len(models)-5)
			break
		}
		fmt.Fprintf(&b, "  %-40q %d\n", m, r.UncostedByModel[m])
	}
	share := 0.0
	if r.TTLWriteTokens > 0 {
		share = 100 * float64(r.TTLUnknownTokens) / float64(r.TTLWriteTokens)
	}
	fmt.Fprintf(&b, "TTL unknown tokens: %d of %d cache-write tokens (%.1f%%)\n", r.TTLUnknownTokens, r.TTLWriteTokens, share)
	if r.FailedCalls > 0 {
		fmt.Fprintf(&b, "INCOMPLETE: %d CLI calls failed after retries; totals above are missing data\n", r.FailedCalls)
	}
	for _, f := range r.Findings {
		b.WriteString(f + "\n")
	}
	return b.String()
}

// fetchRuns pages newest-first through `page` until a page holds a run older
// than `cutoff` or no cursor is left, and returns the runs created at or after
// `cutoff`.
func fetchRuns(page func(before string) ([]Run, string, error), cutoff time.Time) ([]Run, error) {
	var out []Run
	before := ""
	for {
		runs, next, err := page(before)
		if err != nil {
			return nil, err
		}
		crossed := false
		for _, r := range runs {
			if r.CreatedAt.Before(cutoff) {
				crossed = true
				continue
			}
			out = append(out, r)
		}
		if crossed || next == "" {
			return out, nil
		}
		before = next
	}
}
