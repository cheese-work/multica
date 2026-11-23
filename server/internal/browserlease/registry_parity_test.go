package browserlease

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// servedClasses is what the reader lets an instance row serve, read through the registry.
func servedClasses(t *testing.T, e *env, name string) []string {
	t.Helper()
	var out []string
	for _, class := range []string{ClassS0, ClassS2, ClassH} {
		ins, err := NameRegistry{Queries: e.svc.Queries}.Instances(context.Background(), e.ws, class)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range ins {
			if in.Name == name {
				out = append(out, class)
			}
		}
	}
	return out
}

// TestMintParityTable: the reader serves a row only when the name rule and the columns agree and
// the classes are exactly the legacy set of the host; any other row serves nothing (fail closed).
func TestMintParityTable(t *testing.T) {
	for _, tc := range []struct {
		name, host, gateway string
		classes             []string
		want                []string // classes the row serves
	}{
		{"x99-1", "x99", "x99", []string{"S0"}, []string{"S0"}},
		{"c00-1", "c00", "c00", []string{"S2", "H"}, []string{"S2", "H"}},
		{"c00-2", "c00", "c00", []string{"H", "S2"}, []string{"S2", "H"}}, // order is not meaning
		{"x99-name-with-more-dashes-2", "x99", "x99", []string{"S0"}, []string{"S0"}},
		// Name and host column disagree: both directions skipped.
		{"x99-3", "c00", "c00", []string{"S2", "H"}, nil},
		{"c00-3", "x99", "x99", []string{"S0"}, nil},
		// Subset of the legacy classes: outside the legacy contract.
		{"c00-4", "c00", "c00", []string{"S2"}, nil},
		{"c00-5", "c00", "c00", []string{"H"}, nil},
		// Foreign prefix, lookalike prefix, no separator.
		{"h3-1", "c00", "c00", []string{"S2", "H"}, nil},
		{"x99x-1", "x99", "x99", []string{"S0"}, nil},
		{"x99", "x99", "x99", []string{"S0"}, nil},
		{"X99-1", "x99", "x99", []string{"S0"}, nil},
	} {
		t.Run(tc.name+"/"+tc.host+"/"+strings.Join(tc.classes, ","), func(t *testing.T) {
			e := newEnv(t)
			e.register(tc.name, tc.host, tc.gateway, tc.classes)
			got := servedClasses(t, e, tc.name)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("row %s/%s/%s %v serves %v, want %v", tc.name, tc.host, tc.gateway, tc.classes, got, want)
			}
		})
	}
}

// TestReaderSkipsNameColumnDisagreement: an instance whose name says one host and whose columns
// say the other is never granted, and a mint with nothing else to serve it gets ErrNoInstance.
func TestReaderSkipsNameColumnDisagreement(t *testing.T) {
	e := newEnv(t)
	e.register("x99-1", "c00", "c00", []string{"S2", "H"})
	e.register("c00-1", "x99", "x99", []string{"S0"})
	e.credential("c00")
	e.credential("x99")
	for _, class := range []string{ClassS0, ClassS2} {
		if _, err := e.svc.Mint(context.Background(), e.req(e.profile(class))); !errors.Is(err, ErrNoInstance) {
			t.Fatalf("class %s: err = %v, want ErrNoInstance", class, err)
		}
	}
	var n int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM browser_lease WHERE workspace_id = $1`, e.ws).Scan(&n)
	if n != 0 || len(e.signer.claims) != 0 {
		t.Fatalf("leases = %d, tokens = %d, want none", n, len(e.signer.claims))
	}
}

// m1Valid is the reference model of a row the M1 migration accepts (plan r16 T3): the host is a
// legacy host, the classes are exactly its legacy set, the name starts with `host-`, and the
// gateway is the host. It is written from the plan, not from the reader.
func m1Valid(name, host, gateway string, classes []string) bool {
	legacy := map[string][]string{"x99": {"S0"}, "c00": {"S2", "H"}}
	want, ok := legacy[host]
	if !ok || gateway != host || !strings.HasPrefix(name, host+"-") {
		return false
	}
	have := slices.Clone(classes)
	slices.Sort(have)
	have = slices.Compact(have)
	slices.Sort(want)
	return slices.Equal(have, want)
}

// TestReadersAgreeOnEveryM1ValidRow: over every combination of name, host, gateway and class set
// in the enumerated space, the parity reader accepts exactly the M1-valid rows and serves exactly
// their host's legacy classes. T5 extends this test with the row reader (RowRegistry).
func TestReadersAgreeOnEveryM1ValidRow(t *testing.T) {
	names := []string{"x99-1", "c00-1", "h3-1", "x99-", "x99", "xx99-1", "c00x-1", ""}
	hosts := []string{"x99", "c00", "h3", ""}
	classSets := [][]string{
		{}, {"S0"}, {"S2"}, {"H"}, {"S0", "S2"}, {"S2", "H"}, {"H", "S2"}, {"S2", "S2", "H"},
		{"S0", "S2", "H"}, {"S1"}, {"S2", "H", "S1"},
	}
	accepted := 0
	for _, name := range names {
		for _, host := range hosts {
			for _, gateway := range hosts {
				for _, classes := range classSets {
					row := db.BrowserInstance{Name: name, Host: host, Gateway: gateway, AllowedClasses: classes}
					got, ok := parityHost(row)
					want := m1Valid(name, host, gateway, classes)
					if ok != want {
						t.Fatalf("row %s", fmt.Sprintf("%+v", row))
					}
					if !ok {
						continue
					}
					accepted++
					if got != host {
						t.Fatalf("row %+v: host = %q, want %q", row, got, host)
					}
					for _, class := range []string{ClassS0, "S1", ClassS2, ClassH} {
						if serves := parityServes(row, class); serves != slices.Contains(legacyClassesOf(host), class) {
							t.Fatalf("row %+v class %s: serves = %v", row, class, serves)
						}
					}
				}
			}
		}
	}
	if accepted == 0 {
		t.Fatal("the enumerated space holds no M1-valid row")
	}
}
