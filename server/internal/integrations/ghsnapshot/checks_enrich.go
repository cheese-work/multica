package ghsnapshot

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// fetchEnrichedSnapshot is the production snapshot fetcher.
func fetchEnrichedSnapshot(ctx context.Context, c *Client, installationID int64, owner, repo string, number int32) (*PRSnapshot, error) {
	snap, err := FetchPRSnapshot(ctx, c, installationID, owner, repo, number)
	if err == nil {
		EnrichChecks(ctx, c, installationID, owner, repo, snap)
	}
	return snap, err
}

// EnrichChecks sets Required and RedOnBase on the failing checks of snap
// (CHE-3257). Both reads are best effort and fail closed: if the branch rules
// cannot be read every check counts as required, and if the base tip cannot be
// read no check counts as red there, so a red keeps routing to the author.
// Checks match by name only, not by app id.
func EnrichChecks(ctx context.Context, c *Client, installationID int64, owner, repo string, snap *PRSnapshot) {
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	required, ok := requiredContexts(ctx, c, installationID, base, snap.BaseRef)
	needBase := false
	for i := range snap.Contexts {
		cc := &snap.Contexts[i]
		cc.Required = !ok || required[cc.Name]
		needBase = needBase || (cc.Required && cc.Status == "completed" && rollupConclusion(cc.Conclusion) == "FAILURE")
	}
	if !needBase || snap.BaseSHA == "" {
		return
	}
	red := redContexts(ctx, c, installationID, base, snap.BaseSHA)
	for i := range snap.Contexts {
		snap.Contexts[i].RedOnBase = red[snap.Contexts[i].Name]
	}
}

// requiredContexts is the union of the status checks every rule on the base
// branch requires.
func requiredContexts(ctx context.Context, c *Client, installationID int64, base, ref string) (map[string]bool, bool) {
	if ref == "" {
		return nil, false
	}
	data, err := c.restGet(ctx, installationID, base+"/rules/branches/"+url.PathEscape(ref))
	if err != nil {
		return nil, false
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &rules) != nil {
		return nil, false
	}
	out := map[string]bool{}
	for _, r := range rules {
		if r.Type != "required_status_checks" {
			continue
		}
		for _, chk := range r.Parameters.Checks {
			out[chk.Context] = true
		}
	}
	return out, true
}

// redContexts names the checks failing on commit sha.
func redContexts(ctx context.Context, c *Client, installationID int64, base, sha string) map[string]bool {
	commit := base + "/commits/" + url.PathEscape(sha)
	runs, err := fetchSnapshotPages[restCheckRun](ctx, c, installationID, commit+"/check-runs?filter=latest", "check_runs")
	if err != nil {
		return nil
	}
	statuses, err := fetchSnapshotPages[restStatusContext](ctx, c, installationID, commit+"/status", "statuses")
	if err != nil {
		return nil
	}
	red := map[string]bool{}
	for _, r := range runs {
		if normalizeRunStatus(r.Status) == "completed" && rollupConclusion(strings.ToLower(r.Conclusion)) == "FAILURE" {
			red[r.Name] = true
		}
	}
	for _, s := range statuses {
		if _, conclusion := normalizeStatusState(s.State); rollupConclusion(conclusion) == "FAILURE" {
			red[s.Context] = true
		}
	}
	return red
}
