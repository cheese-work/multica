package metrics

import (
	"math"
	"testing"
)

func i64p(v int64) *int64 { return &v }

// Real run, sampled from an X99 Claude Code transcript (task workdir
// che-7705, session 3f977a01, model claude-sonnet-5-5): 9 de-duplicated
// main-loop assistant messages. Every written token carried the 1-hour TTL
// (ephemeral_1h_input_tokens == cache_creation_input_tokens, 5m == 0).
//
// Transcript price at the vendor rates (platform.claude.com/docs/en/
// about-claude/pricing, accessed 2026-11-20): input $2, 5m write $2.50,
// 1h write $4, read $0.10, output $10 per MTok:
//
//	(20*2 + 369925*0.10 + 42685*4 + 5024*10) / 1e6 = 0.2580125
const (
	sampleInput   = 20
	sampleOutput  = 5024
	sampleRead    = 369925
	sampleWrite   = 42685
	sampleWrite1h = 42685
	samplePrice   = 0.2580125
)

func within(got, want, frac float64) bool {
	return math.Abs(got-want) <= want*frac
}

func TestEstimateUSDMatchesTranscriptPrice(t *testing.T) {
	price, ok := PriceForModelAlias("claude-sonnet-5-5")
	if !ok {
		t.Fatal("claude-sonnet-5-5 not priced")
	}
	got := price.EstimateUSD(sampleInput, sampleOutput, sampleRead, sampleWrite, i64p(sampleWrite1h))
	if !within(got, samplePrice, 0.02) {
		t.Fatalf("estimate %.6f, transcript price %.6f: off by more than 2%%", got, samplePrice)
	}
}

// A NULL 1-hour value is "no split reported": every written token prices at
// today's 5-minute rate, exactly as before the column existed.
func TestEstimateUSDNilOneHourPricesAsToday(t *testing.T) {
	price, _ := PriceForModelAlias("claude-sonnet-5-5")
	got := price.EstimateUSD(sampleInput, sampleOutput, sampleRead, sampleWrite, nil)
	want := (float64(sampleInput)*price.InputPerM +
		float64(sampleOutput)*price.OutputPerM +
		float64(sampleRead)*price.CacheReadPerM +
		float64(sampleWrite)*price.CacheWritePerM) / 1e6
	if !within(got, want, 1e-12) {
		t.Fatalf("nil 1h estimate = %v, want today's %v", got, want)
	}
}

func TestCacheWriteCostUSDSplit(t *testing.T) {
	price, _ := PriceForModelAlias("claude-opus-5-5") // 5m $5, 1h $8
	if got, want := price.CacheWriteCostUSD(1_000_000, i64p(400_000)), 0.4*8+0.6*5; !within(got, want, 1e-9) {
		t.Fatalf("mixed split = %v, want %v", got, want)
	}
	// 0 is a real "all 5-minute", not NULL-like.
	if got, want := price.CacheWriteCostUSD(1_000_000, i64p(0)), 5.0; !within(got, want, 1e-9) {
		t.Fatalf("zero 1h = %v, want %v", got, want)
	}
	// A 1-hour count above the total (bad data) clamps to the total.
	if got, want := price.CacheWriteCostUSD(1_000_000, i64p(2_000_000)), 8.0; !within(got, want, 1e-9) {
		t.Fatalf("over-total 1h = %v, want %v", got, want)
	}
	// A negative count clamps to 0.
	if got, want := price.CacheWriteCostUSD(1_000_000, i64p(-5)), 5.0; !within(got, want, 1e-9) {
		t.Fatalf("negative 1h = %v, want %v", got, want)
	}
}

// Rows with no published 1-hour rate (every non-Anthropic SKU) bill the whole
// write at CacheWritePerM, whatever the split says.
func TestCacheWriteCostUSDNoOneHourRate(t *testing.T) {
	price, ok := PriceForModelAlias("gpt-5.6-terra")
	if !ok {
		t.Fatal("gpt-5.6-terra not priced")
	}
	if price.CacheWrite1hPerM != 0 {
		t.Fatalf("gpt-5.6-terra CacheWrite1hPerM = %v, want 0", price.CacheWrite1hPerM)
	}
	got := price.CacheWriteCostUSD(1_000_000, i64p(1_000_000))
	if want := price.CacheWritePerM; !within(got, want, 1e-9) {
		t.Fatalf("no 1h rate = %v, want %v", got, want)
	}
}

// Every Anthropic row carries the vendor's 1-hour write rate, which the price
// page lists at 2x base input for all of them.
func TestAnthropicRowsCarryTwoTimesInputOneHourRate(t *testing.T) {
	n := 0
	for key, p := range modelPrices {
		if p.Provider != "anthropic" {
			continue
		}
		n++
		if want := 2 * p.InputPerM; p.CacheWrite1hPerM != want {
			t.Errorf("%s CacheWrite1hPerM = %v, want 2x input %v", key, p.CacheWrite1hPerM, want)
		}
	}
	if n == 0 {
		t.Fatal("no anthropic rows")
	}
}

// claude-fable-5-2 is on the vendor price page ($8 / $10 / $16 / $0.20 / $40,
// accessed 2026-11-20), so it gets its own row.
func TestFable52ResolvesWithOwnRow(t *testing.T) {
	want := ModelPrice{Provider: "anthropic", Model: "claude-fable-5-2", InputPerM: 8.00, CacheReadPerM: 0.20, CacheWritePerM: 10.00, CacheWrite1hPerM: 16.00, OutputPerM: 40.00}
	for _, id := range []string{
		"claude-fable-5-2", "claude-fable-5.2", "claude-fable-5-2[1m]",
		"anthropic/claude-fable-5-2", "claude-fable-5-2-20261101", "claude-fable-5-2-latest",
	} {
		got, ok := PriceForModelAlias(id)
		if !ok {
			t.Errorf("PriceForModelAlias(%q) did not resolve", id)
			continue
		}
		if got != want {
			t.Errorf("PriceForModelAlias(%q) = %+v, want %+v", id, got, want)
		}
	}
	// Neighbours keep their own rows and unknown minors stay unmapped.
	if got, _ := PriceForModelAlias("claude-fable-5-1"); got.Model != "claude-fable-5-1" {
		t.Errorf("fable 5.1 resolved to %+v", got)
	}
	for _, id := range []string{"claude-fable-5-20", "claude-fable-5-2x", "claude-fable-5-2[1m][2m]", "claude-fable-5-3"} {
		if got, ok := PriceForModelAlias(id); ok {
			t.Errorf("PriceForModelAlias(%q) resolved to %+v; want unmapped", id, got)
		}
	}
}
