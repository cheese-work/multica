package mint

import (
	"reflect"
	"testing"
)

func TestIntersect(t *testing.T) {
	ceiling := Permissions{"contents": "read", "actions": "read", "checks": "read", "metadata": "read"}
	entry := Permissions{"contents": "read", "actions": "read", "issues": "read", "checks": "write"}
	cases := []struct {
		name   string
		wanted Permissions
		want   Permissions
	}{
		{"both allow", Permissions{"contents": "read"}, Permissions{"contents": "read"}},
		{"name outside entry dropped", Permissions{"contents": "read", "statuses": "read"}, Permissions{"contents": "read"}},
		{"name outside ceiling dropped", Permissions{"issues": "read", "contents": "read"}, Permissions{"contents": "read"}},
		{"write wanted, read in entry and ceiling", Permissions{"contents": "write"}, Permissions{"contents": "read"}},
		{"write in entry, read in ceiling", Permissions{"checks": "write"}, Permissions{"checks": "read"}},
		{"lower want kept", Permissions{"actions": "read"}, Permissions{"actions": "read"}},
		{"empty wanted means entry and ceiling", nil, Permissions{"contents": "read", "actions": "read", "checks": "read"}},
		{"unknown level dropped", Permissions{"contents": "root"}, Permissions{}},
		{"nothing in common", Permissions{"statuses": "read"}, Permissions{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Intersect(c.wanted, entry, ceiling)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Intersect = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIntersectNeverRaisesAboveEntryOrCeiling(t *testing.T) {
	for _, lvl := range []string{"read", "write", "admin"} {
		got := Intersect(Permissions{"contents": lvl}, Permissions{"contents": "admin"}, Permissions{"contents": "read"})
		if got["contents"] != "read" {
			t.Fatalf("wanted %s: got %v, want read", lvl, got)
		}
	}
}
