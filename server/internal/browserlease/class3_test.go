package browserlease

import (
	"context"
	"errors"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestClass3MatchIntersectsTheHostPartOnly(t *testing.T) {
	d := testClass3(t)
	cases := []struct {
		name  string
		hosts []string
		deny  bool
	}{
		{"exact", []string{"paypal.com"}, true},
		{"subdomain of a listed host", []string{"www.paypal.com"}, true},
		{"parent of a listed host", []string{"stripe.com"}, true},
		{"case and trailing dot", []string{"PayPal.COM."}, true},
		{"url form", []string{"https://paypal.com/login"}, true},
		{"port form", []string{"mail.google.com:443"}, true},
		{"one denied among allowed", []string{"example.com", "paypal.com"}, true},
		{"unrelated", []string{"example.com"}, false},
		{"suffix that is not a subdomain", []string{"notpaypal.com"}, false},
		{"pattern host stays allowed", []string{"github.com"}, false},
		{"sibling of a listed host", []string{"docs.google.com"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := d.Match(c.hosts)
			if got != c.deny {
				t.Fatalf("Match(%v) = %v, want %v", c.hosts, got, c.deny)
			}
		})
	}
}

func TestParseClass3DenyKeepsHostsAndSkipsPatterns(t *testing.T) {
	d, err := ParseClass3Deny(strings.NewReader("# c\n[patterns]\ngithub.com/settings/keys\n[hosts]\n\n paypal.com \n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Match([]string{"paypal.com"}); !ok {
		t.Fatal("hosts section entry not loaded")
	}
	if _, ok := d.Match([]string{"github.com"}); ok {
		t.Fatal("patterns section entry leaked into the host list")
	}
}

func TestParseClass3DenyRejectsAFileWithNoHosts(t *testing.T) {
	if _, err := ParseClass3Deny(strings.NewReader("[patterns]\ngithub.com/settings/keys\n")); err == nil {
		t.Fatal("a list with no host part must not load: an empty list denies nothing")
	}
}

func TestMintRefusesAnS2ProfileWithAClass3Host(t *testing.T) {
	e := newEnv(t, "c00-1")
	p := e.profileWithHosts(ClassS2, "paypal.com")
	if _, err := e.svc.Mint(context.Background(), e.req(p)); !errors.Is(err, ErrClass3Denied) {
		t.Fatalf("err = %v, want ErrClass3Denied", err)
	}
}

func TestMintRefusesAnS2ProfileWithNoRecordedHosts(t *testing.T) {
	e := newEnv(t, "c00-1")
	p := e.profileWithHosts(ClassS2)
	if _, err := e.svc.Mint(context.Background(), e.req(p)); !errors.Is(err, ErrClass3Unchecked) {
		t.Fatalf("err = %v, want ErrClass3Unchecked", err)
	}
}

func TestMintRefusesS2WhenNoClass3ListIsLoaded(t *testing.T) {
	e := newEnv(t, "c00-1")
	e.svc.Class3 = nil
	if _, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS2))); !errors.Is(err, ErrClass3Unchecked) {
		t.Fatalf("err = %v, want ErrClass3Unchecked", err)
	}
}

func TestMintAllowsAnS2ProfileWithAnAllowedHost(t *testing.T) {
	e := newEnv(t, "c00-1")
	res, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS2)))
	if err != nil || res.Token == "" {
		t.Fatalf("mint: token=%q err=%v", res.Token, err)
	}
}

func TestMintIgnoresTheClass3ListForS0(t *testing.T) {
	e := newEnv(t, "x99-1")
	e.svc.Class3 = nil
	if _, err := e.svc.Mint(context.Background(), e.req(e.profile(ClassS0))); err != nil {
		t.Fatalf("S0 asks no class-3 check: %v", err)
	}
}

func TestRenewEndsTheLeaseWhenTheListGrowsToCoverTheProfile(t *testing.T) {
	e := newEnv(t, "c00-1")
	r := e.req(e.profile(ClassS2))
	res, err := e.svc.Mint(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseClass3Deny(strings.NewReader("[hosts]\nexample.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Class3 = d
	if _, err := e.svc.Renew(context.Background(), e.ws, r.TaskID, res.Lease.ID); !errors.Is(err, ErrClass3Denied) {
		t.Fatalf("renew err = %v, want ErrClass3Denied", err)
	}
	l, err := e.svc.Queries.GetBrowserLease(context.Background(), db.GetBrowserLeaseParams{ID: res.Lease.ID, WorkspaceID: e.ws})
	if err != nil || l.State != "ended" {
		t.Fatalf("lease state = %q err=%v, want ended", l.State, err)
	}
}

func TestProfileHostsAreRecordedAtCreation(t *testing.T) {
	e := newEnv(t)
	p := e.profileWithHosts(ClassS2, "example.com", "app.example.com")
	got, err := e.svc.Queries.GetBrowserProfile(context.Background(), db.GetBrowserProfileParams{ID: p.ID, WorkspaceID: e.ws})
	if err != nil || len(got.Hosts) != 2 || got.Hosts[1] != "app.example.com" {
		t.Fatalf("hosts = %v err=%v", got.Hosts, err)
	}
}
