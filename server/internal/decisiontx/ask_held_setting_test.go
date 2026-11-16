package decisiontx

import (
	"encoding/json"
	"strings"
	"testing"
)

// CHE-5951: an ask may not ask a person for a setting the workspace settings
// already hold. The ask names the keys in the typed field settings_needed; the
// server never reads the prose (CHE-3345).

const heldSettings = `{"github_wake_on_pr_merge": true, "compact_prompt": "keep it short", "empty_text": "", "empty_list": [], "nothing": null, "mode_key": "x"}`

func askWithSettings(test *testing.T, kind string, keys ...any) Ask {
	test.Helper()
	m := validAskMap()
	if kind == KindAction {
		m = validActionAskMap()
	}
	if keys != nil {
		m["settings_needed"] = keys
	}
	raw, _ := json.Marshal(m)
	ask, err := DecodeAskKind(raw, kind)
	if err != nil {
		test.Fatalf("decode: %v", err)
	}
	return ask
}

func TestHeldModeFromSettings(test *testing.T) {
	cases := []struct {
		name, raw string
		want      HeldMode
		wantErr   string
	}{
		{"empty", ``, "off", ""},
		{"absent", `{"a":1}`, "off", ""},
		{"off", `{"held_setting_ask_mode":"off"}`, "off", ""},
		{"warn", `{"held_setting_ask_mode":"warn"}`, "warn", ""},
		{"block", `{"held_setting_ask_mode":"block"}`, "block", ""},
		{"bad value", `{"held_setting_ask_mode":"loud"}`, "", "held_setting_ask_mode must be off, warn or block"},
		{"null", `{"held_setting_ask_mode":null}`, "", "held_setting_ask_mode must be off, warn or block"},
		{"not json", `{`, "", "workspace settings"},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			got, err := HeldModeFromSettings([]byte(tc.raw))
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				test.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if got != tc.want {
				test.Fatalf("mode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckHeldSettingsBlock(test *testing.T) {
	cases := []struct {
		name string
		keys []any
		want string
	}{
		{"held bool", []any{"github_wake_on_pr_merge"}, `ask.settings_needed[0] "github_wake_on_pr_merge" is already set in workspace settings; read it with multica workspace get, do not ask a person`},
		{"held text", []any{"compact_prompt"}, `ask.settings_needed[0] "compact_prompt" is already set in workspace settings; read it with multica workspace get, do not ask a person`},
		{"second key held", []any{"not_there", "compact_prompt"}, `ask.settings_needed[1] "compact_prompt" is already set in workspace settings; read it with multica workspace get, do not ask a person`},
		{"case folded", []any{"Compact_Prompt"}, `ask.settings_needed[0] "Compact_Prompt" is already set in workspace settings; read it with multica workspace get, do not ask a person`},
	}
	for _, kind := range []string{KindDecision, KindAction} {
		for _, tc := range cases {
			test.Run(kind+"/"+tc.name, func(test *testing.T) {
				ask := askWithSettings(test, kind, tc.keys...)
				_, err := ask.CheckHeldSettings("block", []byte(heldSettings))
				if got := errString(err); got != tc.want {
					test.Fatalf("error = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestCheckHeldSettingsAccepts(test *testing.T) {
	cases := []struct {
		name string
		mode HeldMode
		keys []any
	}{
		{"no keys declared", "block", nil},
		{"key absent", "block", []any{"not_there"}},
		{"empty string is not held", "block", []any{"empty_text"}},
		{"empty list is not held", "block", []any{"empty_list"}},
		{"null is not held", "block", []any{"nothing"}},
		{"mode off", "off", []any{"compact_prompt"}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			ask := askWithSettings(test, KindDecision, tc.keys...)
			hints, err := ask.CheckHeldSettings(tc.mode, []byte(heldSettings))
			if err != nil || len(hints) != 0 {
				test.Fatalf("hints = %v, err = %v; want none", hints, err)
			}
		})
	}
}

func TestCheckHeldSettingsWarnHintsAndKeepsTheAsk(test *testing.T) {
	ask := askWithSettings(test, KindDecision, "compact_prompt", "github_wake_on_pr_merge", "not_there")
	hints, err := ask.CheckHeldSettings("warn", []byte(heldSettings))
	if err != nil || len(hints) != 2 {
		test.Fatalf("hints = %v, err = %v; want 2 hints and no error", hints, err)
	}
	if !strings.Contains(hints[0], `"compact_prompt"`) || !strings.Contains(hints[1], `"github_wake_on_pr_merge"`) {
		test.Fatalf("hints = %v", hints)
	}
}

func TestCheckHeldSettingsNeverEchoesTheValue(test *testing.T) {
	ask := askWithSettings(test, KindDecision, "compact_prompt")
	_, err := ask.CheckHeldSettings("block", []byte(heldSettings))
	if err == nil || strings.Contains(err.Error(), "keep it short") {
		test.Fatalf("error = %v; must refuse without the stored value", err)
	}
}

func TestDecodeSettingsNeededRefusesBadShape(test *testing.T) {
	cases := []struct {
		name string
		keys any
		want string
	}{
		{"not a list", "compact_prompt", "ask.settings_needed must be a list of strings"},
		{"blank", []any{" "}, "ask.settings_needed[0] must be a setting key of letters, digits, _ . -"},
		{"spaces", []any{"two words"}, "ask.settings_needed[0] must be a setting key of letters, digits, _ . -"},
		{"duplicate", []any{"a_b", "A_B"}, "ask.settings_needed[1] repeats an earlier key"},
		{"too many", []any{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10", "a11"}, "ask.settings_needed takes at most 10 keys"},
	}
	for _, kind := range []string{KindDecision, KindAction} {
		for _, tc := range cases {
			test.Run(kind+"/"+tc.name, func(test *testing.T) {
				m := validAskMap()
				if kind == KindAction {
					m = validActionAskMap()
				}
				m["settings_needed"] = tc.keys
				raw, _ := json.Marshal(m)
				_, err := DecodeAskKind(raw, kind)
				if got := errString(err); got != tc.want {
					test.Fatalf("error = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

func TestSettingsNeededKeepsOldAskFingerprint(test *testing.T) {
	raw, _ := json.Marshal(decodedAsk(test))
	if strings.Contains(string(raw), "settings_needed") {
		test.Fatalf("an ask without the field must not serialize it: %s", raw)
	}
}
