package decisionaction

import "testing"

func TestCanonical(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"sorts keys and strips space", `{ "b": 1, "a": [true, null, "x"] }`, `{"a":[true,null,"x"],"b":1}`},
		{"nested objects sorted", `{"z":{"b":2,"a":1}}`, `{"z":{"a":1,"b":2}}`},
		{"keys sort by UTF-16 code unit", "{\"\U0001F600\":1,\"דּ\":2}", "{\"\U0001F600\":1,\"דּ\":2}"},
		{"minimal escapes, no HTML escaping", `{"s":"<a>&\u0001\n\"\\ é"}`, "{\"s\":\"<a>&\\u0001\\n\\\"\\\\ é\"}"},
		{"negative zero integer", `{"n":-0}`, `{"n":0}`},
		{"empty containers", `{"a":{},"b":[]}`, `{"a":{},"b":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonical([]byte(tc.in))
			if err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCanonicalRefuses(t *testing.T) {
	for name, in := range map[string]string{
		"fraction":          `{"n":1.5}`,
		"exponent":          `{"n":1e3}`,
		"integer past 2^53": `{"n":9007199254740992}`,
		"duplicate key":     `{"a":1,"a":2}`,
		"trailing data":     `{"a":1} {"b":2}`,
		"not json":          `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := Canonical([]byte(in)); err == nil {
				t.Fatalf("want error, got %s", got)
			}
		})
	}
}
