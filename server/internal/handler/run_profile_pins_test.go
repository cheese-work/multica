package handler

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Pinned commits on the claim (CHE-2216 plan 3.5, slice B7a).

func TestGrantPins(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	id := func(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }
	grant := func(b byte, base, head string) db.RunCapabilityGrant {
		g := db.RunCapabilityGrant{ID: id(b)}
		if base != "" {
			g.PinnedBaseSha = pgtype.Text{String: base, Valid: true}
		}
		if head != "" {
			g.PinnedHeadSha = pgtype.Text{String: head, Valid: true}
		}
		return g
	}
	a, b := sha("a"), sha("b")
	cases := []struct {
		name       string
		grants     []db.RunCapabilityGrant
		frozen     []pgtype.UUID
		base, head string
	}{
		{"no grant", nil, nil, "", ""},
		{"unpinned grant", []db.RunCapabilityGrant{grant(1, "", "")}, nil, "", ""},
		{"base only", []db.RunCapabilityGrant{grant(1, a, "")}, nil, a, ""},
		{"both pins", []db.RunCapabilityGrant{grant(1, a, b)}, nil, a, b},
		{"same pin on two grants", []db.RunCapabilityGrant{grant(1, a, ""), grant(2, a, "")}, nil, a, ""},
		{"pin on one of two grants", []db.RunCapabilityGrant{grant(1, "", ""), grant(2, a, "")}, nil, a, ""},
		{"conflicting base pins fail closed", []db.RunCapabilityGrant{grant(1, a, b), grant(2, b, "")}, nil, "", ""},
		{"conflicting head pins fail closed", []db.RunCapabilityGrant{grant(1, a, a), grant(2, a, b)}, nil, "", ""},
		{"a grant added after the freeze is ignored", []db.RunCapabilityGrant{grant(1, a, ""), grant(2, b, b)}, []pgtype.UUID{id(1)}, a, ""},
	}
	for _, c := range cases {
		base, head := grantPins(c.grants, c.frozen)
		if base != c.base || head != c.head {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, base, head, c.base, c.head)
		}
	}
}
