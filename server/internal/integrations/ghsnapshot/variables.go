package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ErrVariablesPermission means the GitHub App cannot read repository Actions
// variables. The fix is the App's "Variables: read" repository permission,
// which only a human with App admin rights can grant (CHE-5121).
var ErrVariablesPermission = errors.New("github: the App cannot read repository variables (needs the repository permission Variables: read)")

// RepoVariableNames lists the names of a repository's Actions variables. A
// failed or partial read is an error, never an empty list.
func (client *Client) RepoVariableNames(ctx context.Context, installationID int64, owner, repo string) ([]string, error) {
	if !client.Enabled() {
		return nil, errors.New("github: repository variables API is not configured")
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/variables", url.PathEscape(owner), url.PathEscape(repo))
	var names []string
	for page := 1; page <= maxSnapshotContextPages; page++ {
		body, err := client.restGet(ctx, installationID, fmt.Sprintf("%s?per_page=%d&page=%d", path, snapshotPageSize, page))
		if err != nil {
			var status *StatusError
			if errors.As(err, &status) && (status.Status == http.StatusForbidden || status.Status == http.StatusUnauthorized) {
				return nil, ErrVariablesPermission
			}
			return nil, err
		}
		var out struct {
			Variables []struct {
				Name string `json:"name"`
			} `json:"variables"`
		}
		if json.Unmarshal(body, &out) != nil || out.Variables == nil || len(out.Variables) > snapshotPageSize {
			return nil, errors.New("github: malformed repository variables page")
		}
		for _, v := range out.Variables {
			names = append(names, v.Name)
		}
		if len(out.Variables) < snapshotPageSize {
			return names, nil
		}
	}
	return nil, errors.New("github: repository variables pagination limit exceeded")
}
