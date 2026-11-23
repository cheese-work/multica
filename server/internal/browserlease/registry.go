package browserlease

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// NameRegistry is the stub for the W3-18 instance registry: the host comes from the instance
// name prefix (`x99-1`, `c00-1`). X99 instances serve S0 only; C00 instances serve S2 and H.
// W3-18 replaces it with registry columns.
type NameRegistry struct{ Queries *db.Queries }

func hostOf(name string) string {
	switch {
	case strings.HasPrefix(name, HostX99+"-"):
		return HostX99
	case strings.HasPrefix(name, HostC00+"-"):
		return HostC00
	}
	return ""
}

func hostServes(host, class string) bool {
	switch host {
	case HostX99:
		return class == ClassS0
	case HostC00:
		return class == ClassS2 || class == ClassH
	}
	return false
}

// legacyClasses is the exact class set the legacy host matrix gives a host.
func legacyClasses(host string) []string {
	if host == HostX99 {
		return []string{ClassS0}
	}
	return []string{ClassS2, ClassH}
}

// parityHost is the fail-closed parity reader (plan r16, A1c). It returns the host of a row only
// when the name rule and the columns agree: the name starts with `host-`, host and gateway are that
// host, and the classes are exactly the host's legacy set. Anything else (a row the M1 migration
// would refuse, or one written around it) serves nothing.
func parityHost(row db.BrowserInstance) (string, bool) {
	host := hostOf(row.Name)
	if host == "" || row.Host != host || row.Gateway != host {
		return "", false
	}
	want := legacyClasses(host)
	for _, c := range row.AllowedClasses {
		if !slices.Contains(want, c) {
			return "", false
		}
	}
	for _, c := range want {
		if !slices.Contains(row.AllowedClasses, c) {
			return "", false
		}
	}
	return host, true
}

// parityServes reports whether a parity-valid row serves class.
func parityServes(row db.BrowserInstance, class string) bool {
	host, ok := parityHost(row)
	return ok && hostServes(host, class)
}

func (r NameRegistry) Instances(ctx context.Context, ws pgtype.UUID, class string) ([]Instance, error) {
	rows, err := r.Queries.ListBrowserInstances(ctx, ws)
	if err != nil {
		return nil, err
	}
	var out []Instance
	for _, row := range rows {
		if host, ok := parityHost(row); ok && hostServes(host, class) {
			out = append(out, Instance{BrowserInstance: row, Host: host})
		}
	}
	return out, nil
}
