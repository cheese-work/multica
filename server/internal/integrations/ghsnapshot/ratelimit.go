package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// RateLimit reads GET /rate_limit (core bucket) for an installation. GitHub
// documents that this endpoint does not count against the REST rate limit, so
// the CI watcher budget ledger (ghbudget) uses it as its authoritative refresh.
func (client *Client) RateLimit(ctx context.Context, installationID int64) (int, time.Time, error) {
	data, err := client.restGet(ctx, installationID, "/rate_limit")
	if err != nil {
		return 0, time.Time{}, err
	}
	var body struct {
		Resources struct {
			Core *struct {
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Resources.Core == nil || body.Resources.Core.Reset <= 0 {
		return 0, time.Time{}, errors.New("github rate_limit: no core bucket in response")
	}
	return body.Resources.Core.Remaining, time.Unix(body.Resources.Core.Reset, 0), nil
}
