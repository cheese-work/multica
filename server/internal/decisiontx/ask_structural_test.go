package decisiontx

import (
	"strings"
	"testing"
)

func hintsContain(hints []string, part string) bool {
	for _, hint := range hints {
		if strings.Contains(hint, part) {
			return true
		}
	}
	return false
}

// Natural language never refuses an ask: wording that looks like several
// questions decodes and raises an advisory hint instead.
func TestSeveralQuestionWordingIsAdvisory(test *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"ascii question marks", func(ask map[string]any) { ask["decision"] = "Ship now? Or wait?" }},
		{"vietnamese", func(ask map[string]any) { ask["decision"] = "Phát hành ngay? Hay đợi thêm?" }},
		{"chinese full-width", func(ask map[string]any) { ask["decision"] = "现在发布吗？还是再等等？" }},
		{"mixed width", func(ask map[string]any) { ask["decision"] = "现在发布吗？Or wait?" }},
		{"answer 1 to 5", func(ask map[string]any) { ask["decision"] = "Answer 1 to 5 below." }},
		{"decisions 5 and 6", func(ask map[string]any) { ask["decision"] = "Settle decisions 5 and 6." }},
		{"answer per number option", func(ask map[string]any) { optionOf(ask, 0)["label"] = "Answer per number" }},
		{"numbered answers option", func(ask map[string]any) { optionOf(ask, 1)["what_happens"] = "reply with numbered answers" }},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			tc.mutate(ask)
			decoded, err := decodeMap(ask)
			if err != nil {
				test.Fatalf("refused: %v", err)
			}
			if hints := LintAsk(decoded); !hintsContain(hints, "more than one question") {
				test.Fatalf("hints = %q, want a more-than-one-question hint", hints)
			}
		})
	}
}

// One question mark, ASCII or full-width, gives the same outcome: accepted, no hint.
func TestSingleQuestionMarkWidthDoesNotChangeOutcome(test *testing.T) {
	for _, decision := range []string{"Ship now?", "现在发布吗？", "现在发布吗?", "Phát hành ngay?"} {
		decoded, err := decodeMap(func() map[string]any { ask := validAskMap(); ask["decision"] = decision; return ask }())
		if err != nil {
			test.Fatalf("%q refused: %v", decision, err)
		}
		if hints := LintAsk(decoded); hintsContain(hints, "more than one question") {
			test.Fatalf("%q got hints %q", decision, hints)
		}
	}
}

// The agent_did bound counts characters, so a language with no spaces is not too short.
func TestAgentDidLengthCountsCharacters(test *testing.T) {
	accepted := []string{
		"已在10月27日检查三个拉取请求，没有放行任何关卡。",
		"Đã chạy toàn bộ kiểm tra yêu cầu.",
		"Ran all requested checks.",
	}
	for _, text := range accepted {
		if err := decodeWith(func(ask map[string]any) { ask["agent_did"] = text }); err != nil {
			test.Fatalf("agent_did %q refused: %v", text, err)
		}
	}
	for _, text := range []string{"完成。", "Done.", "75eed3bd-33c6-42f5-b612-cf0f03b8879a", "[x](/issue/" + testIssueID + ") 904221117"} {
		err := decodeWith(func(ask map[string]any) { ask["agent_did"] = text })
		if got := errString(err); got != agentDidMsg {
			test.Fatalf("agent_did %q: error = %q, want %q", text, got, agentDidMsg)
		}
	}
}

// An implicit-values word is a hint, never a refusal.
func TestImplicitValueWordIsAdvisory(test *testing.T) {
	for _, action := range []string{"Set the URL, tương tự the others", "Set the URL, etc."} {
		ask := stepsAsk(test, "pass")
		firstStep(ask)["action"] = action
		decoded, err := decodeMap(ask)
		if err != nil {
			test.Fatalf("%q refused: %v", action, err)
		}
		if hints := LintAsk(decoded); !hintsContain(hints, "leaves values implicit") {
			test.Fatalf("%q: hints = %q, want an implicit-values hint", action, hints)
		}
	}
}
