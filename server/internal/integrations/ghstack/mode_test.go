package ghstack

import "testing"

func ws(s string) []byte { return []byte(s) }

// Rows follow the plan's Effective mode rules 1–4.
func TestFinalMode(t *testing.T) {
	const repo = "acme/app"
	tests := []struct {
		name     string
		settings [][]byte
		envKill  bool
		want     Mode
	}{
		{"workspace off, repo link stays off", [][]byte{ws(`{"github_stack_autolink":{"mode":"off","repos":{"acme/app":"link"}}}`)}, false, ModeOff},
		{"repo override up report->link", [][]byte{ws(`{"github_stack_autolink":{"mode":"report","repos":{"acme/app":"link"}}}`)}, false, ModeLink},
		{"repo override down link->off", [][]byte{ws(`{"github_stack_autolink":{"mode":"link","repos":{"acme/app":"off"}}}`)}, false, ModeOff},
		{"repo entry for another repo ignored", [][]byte{ws(`{"github_stack_autolink":{"mode":"link","repos":{"acme/other":"off"}}}`)}, false, ModeLink},
		{"repo key case-insensitive", [][]byte{ws(`{"github_stack_autolink":{"mode":"report","repos":{"Acme/App":"link"}}}`)}, false, ModeLink},
		{"workspace mode link, no repo entry", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`)}, false, ModeLink},
		{"repos without mode defaults to report", [][]byte{ws(`{"github_stack_autolink":{"repos":{"acme/other":"link"}}}`)}, false, ModeReport},
		{"missing key defaults to report", [][]byte{ws(`{"other":1}`)}, false, ModeReport},
		{"empty settings default to report", [][]byte{nil}, false, ModeReport},
		{"malformed settings fail closed", [][]byte{ws(`{"github_stack_autolink":{"mode":"bogus"}}`)}, false, ModeOff},
		{"unparsable json fails closed", [][]byte{ws(`not json`)}, false, ModeOff},
		{"2 workspaces link+report -> report", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`), ws(`{}`)}, false, ModeReport},
		{"2 workspaces link+off -> off", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`), ws(`{"github_stack_autolink":{"mode":"off"}}`)}, false, ModeOff},
		{"2 workspaces link+link -> link", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`), ws(`{"github_stack_autolink":{"mode":"link"}}`)}, false, ModeLink},
		{"3 workspaces link+report+off -> off", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`), ws(`{"github_stack_autolink":{"mode":"report"}}`), ws(`{"github_stack_autolink":{"mode":"off"}}`)}, false, ModeOff},
		{"3 workspaces link+link+report via repo -> report", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`), ws(`{"github_stack_autolink":{"mode":"link"}}`), ws(`{"github_stack_autolink":{"mode":"link","repos":{"acme/app":"report"}}}`)}, false, ModeReport},
		{"no bound workspace fails closed", nil, false, ModeOff},
		{"env kill beats link", [][]byte{ws(`{"github_stack_autolink":{"mode":"link"}}`)}, true, ModeOff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FinalMode(repo, tc.settings, tc.envKill); got != tc.want {
				t.Fatalf("FinalMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveMode(t *testing.T) {
	if got := EffectiveMode("acme/app", ws(`{"github_stack_autolink":{"mode":"link","repos":{"acme/app":"report"}}}`)); got != ModeReport {
		t.Fatalf("EffectiveMode = %q, want report", got)
	}
}

func TestEnvKill(t *testing.T) {
	for v, want := range map[string]bool{"": false, "1": false, "0": true} {
		t.Setenv("GITHUB_STACK_AUTOLINK", v)
		if got := EnvKill(); got != want {
			t.Errorf("GITHUB_STACK_AUTOLINK=%q: EnvKill = %v, want %v", v, got, want)
		}
	}
}

func TestValidateSetting(t *testing.T) {
	ok := []any{
		nil, // not an object: nothing to check, same as ask_guard_mode
		map[string]any{},
		map[string]any{"other": 1},
		map[string]any{SettingKey: map[string]any{}},
		map[string]any{SettingKey: map[string]any{"mode": "link"}},
		map[string]any{SettingKey: map[string]any{"mode": "off", "repos": map[string]any{"acme/app": "link", "a-b/c.d_e": "report"}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{}}},
	}
	for _, v := range ok {
		if err := ValidateSetting(v); err != nil {
			t.Errorf("%v rejected: %v", v, err)
		}
	}
	bad := []any{
		map[string]any{SettingKey: nil},
		map[string]any{SettingKey: "link"},
		map[string]any{SettingKey: []any{}},
		map[string]any{SettingKey: map[string]any{"mode": "strict"}},
		map[string]any{SettingKey: map[string]any{"mode": 1}},
		map[string]any{SettingKey: map[string]any{"mode": nil}},
		map[string]any{SettingKey: map[string]any{"mode": "link", "extra": true}},
		map[string]any{SettingKey: map[string]any{"repos": "acme/app"}},
		map[string]any{SettingKey: map[string]any{"repos": nil}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"acme/app": "strict"}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"acme/app": 1}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"acme": "link"}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"acme/app/x": "link"}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"/app": "link"}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"acme/": "link"}}},
		map[string]any{SettingKey: map[string]any{"repos": map[string]any{"": "link"}}},
	}
	for _, v := range bad {
		if err := ValidateSetting(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
}
