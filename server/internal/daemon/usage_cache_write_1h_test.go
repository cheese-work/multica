package daemon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func ptr64(v int64) *int64 { return &v }

func TestTaskUsageEntriesCarryCacheWrite1h(t *testing.T) {
	t.Parallel()
	entries := taskUsageEntries("claude", map[string]agent.TokenUsage{
		"split": {CacheWriteTokens: 300, CacheWrite1hTokens: ptr64(120)},
		"plain": {CacheWriteTokens: 50},
	})
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	plain, split := entries[0], entries[1] // sorted by model
	if !reflect.DeepEqual(split.CacheWrite1hTokens, ptr64(120)) {
		t.Fatalf("split entry 1h = %v, want 120", split.CacheWrite1hTokens)
	}
	if plain.CacheWrite1hTokens != nil {
		t.Fatalf("plain entry 1h = %v, want nil", *plain.CacheWrite1hTokens)
	}
	if b, _ := json.Marshal(plain); strings.Contains(string(b), "cache_write_1h_tokens") {
		t.Fatalf("nil 1h must be omitted from the payload: %s", b)
	}
	if b, _ := json.Marshal(split); !strings.Contains(string(b), `"cache_write_1h_tokens":120`) {
		t.Fatalf("payload lacks the 1h field: %s", b)
	}
}

func TestMergeUsageSumsCacheWrite1h(t *testing.T) {
	t.Parallel()
	a := map[string]agent.TokenUsage{"m": {CacheWriteTokens: 10, CacheWrite1hTokens: ptr64(4)}, "only-a": {CacheWriteTokens: 1}}
	b := map[string]agent.TokenUsage{"m": {CacheWriteTokens: 20, CacheWrite1hTokens: ptr64(6)}, "only-a2": {CacheWriteTokens: 1}}
	merged := mergeUsage(a, b)
	if got := merged["m"].CacheWrite1hTokens; !reflect.DeepEqual(got, ptr64(10)) {
		t.Fatalf("merged 1h = %v, want 10", got)
	}
	// One side without a split must not erase the other's.
	c := map[string]agent.TokenUsage{"m": {CacheWriteTokens: 5}}
	if got := mergeUsage(a, c)["m"].CacheWrite1hTokens; !reflect.DeepEqual(got, ptr64(4)) {
		t.Fatalf("merge with split-less side = %v, want 4", got)
	}
	// And merging must not mutate the caller's pointer.
	if *a["m"].CacheWrite1hTokens != 4 {
		t.Fatalf("mergeUsage mutated its input: %d", *a["m"].CacheWrite1hTokens)
	}
}
