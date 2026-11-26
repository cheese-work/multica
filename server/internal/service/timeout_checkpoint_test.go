package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseTimeoutCheckpoint_DropsInvalidEntriesKeepsValid(t *testing.T) {
	head := strings.Repeat("A", 40)
	repos := []string{
		fmt.Sprintf(`{"path":"ok","branch":"feat/x","head":%q,"ahead":1,"dirty":0}`, head),
		fmt.Sprintf(`{"path":%q,"branch":"b","head":%q}`, strings.Repeat("p", 121), head), // path too long
		fmt.Sprintf(`{"path":"p","branch":"bad branch","head":%q}`, head),                 // not ref-format
		fmt.Sprintf(`{"path":"p","branch":"b..c","head":%q}`, head),
		`{"path":"p","branch":"b","head":"abc"}`, // not 40-hex
		fmt.Sprintf(`{"path":"p","branch":"b","head":%q,"ahead":-1}`, head),
		`"not an object"`,
	}
	raw := `{"repos":[` + strings.Join(repos, ",") + `],"warning":{"state":"maybe"},"last_actions":[{"tool":"","input":"x"},{"tool":"Bash","input":"ls"}]}`
	cp := ParseTimeoutCheckpoint(json.RawMessage(raw))
	if cp == nil {
		t.Fatal("want a checkpoint")
	}
	if len(cp.Repos) != 1 || cp.Repos[0].Path != "ok" || cp.Repos[0].Head != strings.ToLower(head) {
		t.Fatalf("repos = %+v, want only the valid one with a lowercased head", cp.Repos)
	}
	if len(cp.LastActions) != 1 || cp.LastActions[0].Tool != "Bash" {
		t.Fatalf("actions = %+v, want only the valid one", cp.LastActions)
	}
	if cp.Warning.State != "" {
		t.Fatalf("unknown warning state kept: %+v", cp.Warning)
	}
}

func TestParseTimeoutCheckpoint_Caps(t *testing.T) {
	head := strings.Repeat("a", 40)
	var repos, actions []string
	for i := 0; i < 8; i++ {
		repos = append(repos, fmt.Sprintf(`{"path":"r%d","branch":"b","head":%q}`, i, head))
	}
	for i := 0; i < 12; i++ {
		actions = append(actions, fmt.Sprintf(`{"tool":"T%d","input":%q}`, i, strings.Repeat("x", 300)))
	}
	cp := ParseTimeoutCheckpoint(json.RawMessage(`{"repos":[` + strings.Join(repos, ",") + `],"last_actions":[` + strings.Join(actions, ",") + `],"total_tool_calls":50}`))
	if len(cp.Repos) != 5 {
		t.Fatalf("repos = %d, want 5", len(cp.Repos))
	}
	if len(cp.LastActions) != 8 || cp.LastActions[0].Tool != "T4" || cp.LastActions[7].Tool != "T11" {
		t.Fatalf("actions = %+v, want the last 8 in order", cp.LastActions)
	}
	if n := len([]rune(cp.LastActions[0].Input)); n != 160 {
		t.Fatalf("input length = %d, want 160", n)
	}
	if cp.TotalToolCalls != 50 {
		t.Fatalf("total = %d, want 50", cp.TotalToolCalls)
	}
}

func TestParseTimeoutCheckpoint_NotAnObject(t *testing.T) {
	for _, raw := range []string{``, `null`, `"x"`, `[1]`, `{"repos":"x"}`} {
		if cp := ParseTimeoutCheckpoint(json.RawMessage(raw)); cp != nil {
			t.Errorf("ParseTimeoutCheckpoint(%q) = %+v, want nil", raw, cp)
		}
	}
}

func TestRenderTimeoutCheckpoint_NoActionsAndMentionsNeutralized(t *testing.T) {
	cp := TimeoutCheckpoint{
		Repos:       []TimeoutCheckpointRepo{{Path: "[x](mention://agent/1)", Branch: "b", Head: strings.Repeat("a", 40)}},
		LastActions: nil,
	}
	out := RenderTimeoutCheckpoint(cp, 1)
	if !strings.HasPrefix(out, "Coverage: daemon facts only\n") || !strings.Contains(out, "No tool call recorded") {
		t.Fatalf("render:\n%s", out)
	}
	if strings.Contains(out, "mention://") {
		t.Fatalf("mention link survived:\n%s", out)
	}
}

func TestRenderTimeoutCheckpoint_CappedAt4KB(t *testing.T) {
	cp := TimeoutCheckpoint{TotalToolCalls: 40}
	for i := 0; i < 8; i++ {
		cp.LastActions = append(cp.LastActions, TimeoutCheckpointAction{Tool: "Bash", Input: strings.Repeat("é", 160)})
	}
	for i := 0; i < 5; i++ {
		cp.Repos = append(cp.Repos, TimeoutCheckpointRepo{Path: strings.Repeat("p", 120), Branch: strings.Repeat("b", 200), Head: strings.Repeat("a", 40)})
	}
	out := RenderTimeoutCheckpoint(cp, 2)
	if len(out) > 4096 {
		t.Fatalf("len = %d, want at most 4096", len(out))
	}
}

func TestTimeoutCheckpointRetryPointerForms(t *testing.T) {
	delivered := TimeoutCheckpointRetryPointer(1, "c-1", "Coverage: agent checkpoint requested at 09:25 ICT; daemon facts below\nx")
	if !strings.Contains(delivered, "c-1") || !strings.Contains(delivered, "after the warning at 09:25 ICT") || !strings.Contains(delivered, "Skip only history your own checkpoint covers") {
		t.Fatalf("delivered form:\n%s", delivered)
	}
	other := TimeoutCheckpointRetryPointer(1, "c-2", "Coverage: daemon facts only\nx")
	if !strings.Contains(other, "c-2") || !strings.Contains(other, "normal thread scan") || strings.Contains(other, "own checkpoint") {
		t.Fatalf("daemon-facts form:\n%s", other)
	}
}

func TestParseAndRenderTimeoutCheckpoint_Probe(t *testing.T) {
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn"
	parse := func(probe any) *TimeoutCheckpoint {
		raw, _ := json.Marshal(map[string]any{"repos": []any{}, "last_actions": []any{}, "probe": probe})
		return ParseTimeoutCheckpoint(raw)
	}
	cp := parse("failed timed out after 10s\n`x` " + secret + " " + strings.Repeat("z", 400))
	if !strings.HasPrefix(cp.Probe, "failed timed out after 10s") || strings.Contains(cp.Probe, secret) ||
		strings.ContainsAny(cp.Probe, "`\n") || len([]rune(cp.Probe)) > 160 {
		t.Fatalf("probe = %q, want redacted, one line, at most 160 runes", cp.Probe)
	}
	if out := RenderTimeoutCheckpoint(*cp, 1); !strings.Contains(out, "Git probe: failed timed out after 10s") {
		t.Fatalf("render lacks the probe line:\n%s", out)
	}
	for _, bad := range []any{"ok", "", 5, nil, "[x](mention://agent/1)"} {
		if p := parse(bad); p == nil || p.Probe != "" {
			t.Errorf("probe %v kept as %+v, want dropped", bad, p)
		}
	}
	if out := RenderTimeoutCheckpoint(*parse(nil), 1); strings.Contains(out, "Git probe") {
		t.Fatalf("a successful probe must render no probe line:\n%s", out)
	}
}
