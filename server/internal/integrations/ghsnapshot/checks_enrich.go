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

// EnrichChecks sets RedCause on the failing checks of snap (CHE-3257). A
// failing check that is not required is optional. A required one is infra when
// it is also red on the base tip or has a green run on this same head (a
// re-run), and the PR's own otherwise. Every read is best effort and leaves the
// cause empty (unknown) when it fails: an outage must never be pinned on the
// PR. Checks match by name only, not by app id.
func EnrichChecks(ctx context.Context, c *Client, installationID int64, owner, repo string, snap *PRSnapshot) {
	failing := false
	for _, cc := range snap.Contexts {
		failing = failing || isFailing(cc)
	}
	if !failing {
		return
	}
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	required, ok := requiredContexts(ctx, c, installationID, base, snap.BaseRef)
	if !ok {
		return
	}
	needsEvidence := false
	for _, cc := range snap.Contexts {
		needsEvidence = needsEvidence || (isFailing(cc) && required[cc.Name])
	}
	var onBase, rerunGreen map[string]bool
	if needsEvidence {
		if snap.BaseSHA != "" {
			onBase = redContexts(ctx, c, installationID, base, snap.BaseSHA)
		}
		rerunGreen = greenOnHead(ctx, c, installationID, base, snap.HeadSHA)
	}
	for i := range snap.Contexts {
		cc := &snap.Contexts[i]
		switch {
		case !isFailing(*cc):
		case !required[cc.Name]:
			cc.RedCause = CauseOptional
		case onBase[cc.Name] || rerunGreen[cc.Name]:
			cc.RedCause = CauseInfra
		case onBase != nil && rerunGreen != nil:
			cc.RedCause = CausePR
		}
	}
}

func isFailing(cc CheckContext) bool {
	return cc.Status == "completed" && rollupConclusion(cc.Conclusion) == "FAILURE"
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

// redContexts names the checks failing on commit sha; nil when unreadable.
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

// greenOnHead names the checks with a successful run, in any attempt, on
// commit sha: a check that is red now and green there is flaky. Nil when
// unreadable.
func greenOnHead(ctx context.Context, c *Client, installationID int64, base, sha string) map[string]bool {
	runs, err := fetchSnapshotPages[restCheckRun](ctx, c, installationID, base+"/commits/"+url.PathEscape(sha)+"/check-runs?filter=all", "check_runs")
	if err != nil {
		return nil
	}
	green := map[string]bool{}
	for _, r := range runs {
		if normalizeRunStatus(r.Status) == "completed" && strings.ToLower(r.Conclusion) == "success" {
			green[r.Name] = true
		}
	}
	return green
}
