package service

import "testing"

func TestResolveHarnessProfile(t *testing.T) {
	cases := []struct {
		name, workspace, agent, want string
	}{
		{"nothing set", ``, ``, HarnessProfilePersonal},
		{"empty objects", `{}`, `{}`, HarnessProfilePersonal},
		{"workspace default", `{"harness_profile":"managed"}`, `{}`, HarnessProfileManaged},
		{"agent override beats workspace", `{"harness_profile":"managed"}`, `{"harness_profile":"personal"}`, HarnessProfilePersonal},
		{"agent managed over workspace personal", `{"harness_profile":"personal"}`, `{"harness_profile":"managed"}`, HarnessProfileManaged},
		{"agent null inherits", `{"harness_profile":"managed"}`, `{"harness_profile":null}`, HarnessProfileManaged},
		{"agent empty string inherits", `{"harness_profile":"managed"}`, `{"harness_profile":""}`, HarnessProfileManaged},
		{"unknown agent value falls back to workspace", `{"harness_profile":"managed"}`, `{"harness_profile":"yolo"}`, HarnessProfileManaged},
		{"unknown everywhere is personal", `{"harness_profile":"x"}`, `{"harness_profile":"y"}`, HarnessProfilePersonal},
		{"reserved managed_sandbox is not accepted", `{}`, `{"harness_profile":"managed_sandbox"}`, HarnessProfilePersonal},
		{"garbage json", `not json`, `[1]`, HarnessProfilePersonal},
	}
	for _, c := range cases {
		if got := ResolveHarnessProfile([]byte(c.workspace), []byte(c.agent)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidHarnessProfile(t *testing.T) {
	for _, v := range []string{"personal", "managed"} {
		if !ValidHarnessProfile(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range []string{"", "managed_sandbox", "Managed", "x"} {
		if ValidHarnessProfile(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
}
