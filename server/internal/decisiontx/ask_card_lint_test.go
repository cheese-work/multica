package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
)

// lintAsk is a valid two-option ask; edit mutates the option maps before decode.
func lintAsk(test *testing.T, edit func(first, second map[string]any, ask map[string]any)) Ask {
	test.Helper()
	raw := validAskMap()
	options := raw["options"].([]any)
	edit(options[0].(map[string]any), options[1].(map[string]any), raw)
	body, _ := json.Marshal(raw)
	ask, err := DecodeAsk(body)
	if err != nil {
		test.Fatalf("decode: %v", err)
	}
	return ask
}

func TestCardLintRefusesOneFixturePerCode(test *testing.T) {
	cases := []struct {
		name, class, code, field string
		edit                     func(first, second map[string]any, ask map[string]any)
	}{
		{"weak_cons en time", "reversible", "weak_cons", "ask.options[0].cons", func(a, _ map[string]any, _ map[string]any) {
			a["cons"] = []string{"Takes 1 minute of your time"}
		}},
		{"weak_cons en check", "reversible", "weak_cons", "ask.options[0].cons", func(a, _ map[string]any, _ map[string]any) {
			a["cons"] = []string{"One more check", "Takes 2 minutes of your time"}
		}},
		{"weak_cons vi time", "reversible", "weak_cons", "ask.options[1].cons", func(_, b map[string]any, _ map[string]any) {
			b["cons"] = []string{"Mất 1 phút của bạn"}
		}},
		{"weak_cons vi check", "reversible", "weak_cons", "ask.options[1].cons", func(_, b map[string]any, _ map[string]any) {
			b["cons"] = []string{"Thêm một lần kiểm tra"}
		}},
		{"option_markdown table", "reversible", "option_markdown", "ask.options[0].what_happens", func(a, _ map[string]any, _ map[string]any) {
			a["what_happens"] = "Result:\n| a | b |\n|---|---|\n| 1 | 2 |"
		}},
		{"option_markdown heading", "reversible", "option_markdown", "ask.options[0].what_happens", func(a, _ map[string]any, _ map[string]any) {
			a["what_happens"] = "## Plan\nship it"
		}},
		{"option_markdown image", "reversible", "option_markdown", "ask.options[1].pros", func(_, b map[string]any, _ map[string]any) {
			b["pros"] = []string{"Clear ![shot](http://x/y.png)"}
		}},
		{"option_markdown html", "reversible", "option_markdown", "ask.options[1].cons", func(_, b map[string]any, _ map[string]any) {
			b["cons"] = []string{"Slow<br>and costly"}
		}},
		{"option_markdown fence", "reversible", "option_markdown", "ask.options[0].what_happens", func(a, _ map[string]any, _ map[string]any) {
			a["what_happens"] = "Run:\n```\nmake ship\n```"
		}},
		{"option_markdown mermaid", "reversible", "option_markdown", "ask.options[0].what_happens", func(a, _ map[string]any, _ map[string]any) {
			a["what_happens"] = "~~~mermaid\ngraph TD; A-->B\n~~~"
		}},
		{"option_leak recommended", "reversible", "option_leak", "ask.options[0].label", func(a, _ map[string]any, ask map[string]any) {
			a["label"] = "NOW (recommended)"
		}},
		{"option_leak vi", "reversible", "option_leak", "ask.options[1].label", func(_, b map[string]any, ask map[string]any) {
			b["label"] = "LATER - khuyến nghị"
		}},
		{"option_leak rec", "reversible", "option_leak", "ask.options[0].label", func(a, _ map[string]any, _ map[string]any) {
			a["label"] = "NOW (rec)"
		}},
		{"option_leak pro en", "reversible", "option_leak", "ask.options[0].pros", func(a, _ map[string]any, _ map[string]any) {
			a["pros"] = []string{"Recommended by the team"}
		}},
		{"option_leak con vi", "reversible", "option_leak", "ask.options[1].cons", func(_, b map[string]any, _ map[string]any) {
			b["cons"] = []string{"Không được khuyến nghị"}
		}},
		{"option_leak bold one", "reversible", "option_leak", "ask.options[1].label", func(_, b map[string]any, _ map[string]any) {
			b["label"] = "**LATER**"
		}},
		{"default_leak en", "deploy", "default_leak", "ask.default_if_no_reply", func(_, _ map[string]any, ask map[string]any) {
			ask["default_if_no_reply"] = "LATER"
		}},
		{"default_leak vi", "irreversible", "default_leak", "ask.default_if_no_reply", func(_, _ map[string]any, ask map[string]any) {
			ask["default_if_no_reply"] = "Nếu không trả lời thì chọn NOW"
		}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := lintAsk(test, tc.edit)
			_, err := ask.CheckCard("block", tc.class)
			if err == nil {
				test.Fatal("want refusal")
			}
			got := err.Error()
			if !strings.HasPrefix(got, tc.field+" "+tc.code+":") {
				test.Errorf("error %q: want prefix %q", got, tc.field+" "+tc.code+":")
			}
			if strings.Contains(got, "\n") {
				test.Errorf("error is more than one line: %q", got)
			}
			hints, err := ask.CheckCard("warn", tc.class)
			if err != nil || len(hints) != 1 || !strings.Contains(hints[0], tc.code) {
				test.Errorf("warn: hints %v err %v", hints, err)
			}
			hints, err = ask.CheckCard("off", tc.class)
			if err != nil || hints != nil {
				test.Errorf("off must check nothing: hints %v err %v", hints, err)
			}
		})
	}
}

func TestCardLintAcceptsAGoodCard(test *testing.T) {
	for name, tc := range map[string]struct {
		class string
		edit  func(first, second map[string]any, ask map[string]any)
	}{
		"valid card": {"deploy", func(_, _ map[string]any, ask map[string]any) {
			ask["default_if_no_reply"] = "No action until you pick."
		}},
		"real con beside a weak one": {"reversible", func(a, _ map[string]any, _ map[string]any) {
			a["cons"] = []string{"Takes 1 minute of your time", "Rolls back all CI caches"}
		}},
		"mixed cost en": {"reversible", func(a, _ map[string]any, _ map[string]any) {
			a["cons"] = []string{"Takes 1 minute of your time and permanently deletes the backup"}
		}},
		"mixed cost vi": {"reversible", func(_, b map[string]any, _ map[string]any) {
			b["cons"] = []string{"Mất 1 phút của bạn và xóa vĩnh viễn bản sao lưu"}
		}},
		"allowed markup": {"reversible", func(a, _ map[string]any, _ map[string]any) {
			a["what_happens"] = "**Ships** the *fix* with `Map<String>`.\n- one\n- two\nSee [docs](https://x.test)."
		}},
		"bold on every label": {"reversible", func(a, b map[string]any, _ map[string]any) {
			a["label"], b["label"] = "**NOW**", "**LATER**"
		}},
		"default names an option, reversible": {"reversible", func(_, _ map[string]any, ask map[string]any) {
			ask["default_if_no_reply"] = "LATER"
		}},
		"default phrase with a short label": {"deploy", func(a, _ map[string]any, ask map[string]any) {
			a["label"] = "No"
			ask["default_if_no_reply"] = "No action until you pick."
		}},
	} {
		test.Run(name, func(test *testing.T) {
			ask := lintAsk(test, tc.edit)
			hints, err := ask.CheckCard("block", tc.class)
			if err != nil || len(hints) != 0 {
				test.Fatalf("hints %v err %v", hints, err)
			}
		})
	}
}

func TestCardLintModeFromSettings(test *testing.T) {
	for raw, want := range map[string]string{
		``: "off", `{}`: "off", `{"decision_card_lint_mode":"warn"}`: "warn", `{"decision_card_lint_mode":"block"}`: "block", `{"decision_card_lint_mode":"off"}`: "off",
	} {
		got, err := CardLintModeFromSettings([]byte(raw))
		if err != nil || got != want {
			test.Errorf("%q: got %q err %v, want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{`{"decision_card_lint_mode":"on"}`, `{"decision_card_lint_mode":null}`, `{"decision_card_lint_mode":1}`, `[`} {
		if _, err := CardLintModeFromSettings([]byte(raw)); err == nil {
			test.Errorf("%q: want error", raw)
		}
	}
}

func TestMentionHuman(test *testing.T) {
	const id = "0194a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	got, added := MentionHuman("Pick one.", "Ada [Dev]", id)
	want := "Pick one.\n\n[@Ada Dev](mention://member/" + id + ")"
	if !added || got != want {
		test.Errorf("got %q added %v, want %q", got, added, want)
	}
	have := "Hi [@Ada](mention://member/" + id + ")"
	if got, added := MentionHuman(have, "Ada", id); added || got != have {
		test.Errorf("already mentioned: got %q added %v", got, added)
	}
}
