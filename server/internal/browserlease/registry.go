package browserlease

import (
	"context"
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

// parityHost is the fail-closed parity reader (plan r16, A1c): the host of a row only when the
// name rule and the columns agree and the classes are exactly the host's legacy set.
func parityHost(row db.BrowserInstance) (string, bool) {
	host := hostOf(row.Name)
	return host, host != ""
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
		if host := hostOf(row.Name); hostServes(host, class) {
			out = append(out, Instance{BrowserInstance: row, Host: host})
		}
	}
	return out, nil
}
