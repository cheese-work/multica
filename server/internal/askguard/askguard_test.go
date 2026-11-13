package askguard

import "testing"

func TestModeFromSettings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Mode
		wantErr bool
	}{
		{"missing key defaults to off", `{"other":1}`, ModeOff, false},
		{"empty settings default to off", ``, ModeOff, false},
		{"warn", `{"ask_guard_mode":"warn"}`, ModeWarn, false},
		{"block", `{"ask_guard_mode":"block"}`, ModeBlock, false},
		{"off", `{"ask_guard_mode":"off"}`, ModeOff, false},
		{"unknown value", `{"ask_guard_mode":"strict"}`, "", true},
		{"wrong case", `{"ask_guard_mode":"WARN"}`, "", true},
		{"not a string", `{"ask_guard_mode":true}`, "", true},
		{"settings not an object", `[1]`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ModeFromSettings([]byte(c.raw))
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("ModeFromSettings(%s) = %q, %v; want %q, err=%v", c.raw, got, err, c.want, c.wantErr)
			}
		})
	}
}
