package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ReleaseLabel is the label that marks a release issue. It is attached in its
// own write, after the create, so issue.labels_changed fires for the steward's
// scoped rule (an on-create label would not).
const ReleaseLabel = "release"

// ReleaseDeploy is one verified C00 deploy: the signed intake has already
// checked the workflow, the green readback, and the SHA/digest receipt.
// RepoOwner and RepoName are lowercased.
type ReleaseDeploy struct {
	WorkspaceID         pgtype.UUID
	RepoOwner, RepoName string
	RunID               int64
	OldSHA, NewSHA      string
	Digest              string
}

// CreateRelease opens the release issue for a deploy exactly once. The deploy
// identity (repo, SHA, digest) is claimed first; a redelivery or rerun finds
// the claim and creates nothing, only finishing a label write an earlier try
// left undone. created is false for every duplicate.
func (s *IssueService) CreateRelease(ctx context.Context, d ReleaseDeploy) (issue db.Issue, created bool, err error) {
	key := db.GetGitHubReleaseDeployParams{WorkspaceID: d.WorkspaceID, RepoOwner: d.RepoOwner, RepoName: d.RepoName, SourceSha: d.NewSHA, ImageDigest: d.Digest}
	_, err = s.Queries.ClaimGitHubReleaseDeploy(ctx, db.ClaimGitHubReleaseDeployParams{
		WorkspaceID: d.WorkspaceID, RepoOwner: d.RepoOwner, RepoName: d.RepoName,
		SourceSha: d.NewSHA, ImageDigest: d.Digest, OldSha: d.OldSHA, RunID: d.RunID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.finishDuplicateRelease(ctx, d, key)
	}
	if err != nil {
		return db.Issue{}, false, fmt.Errorf("claim release deploy: %w", err)
	}
	owner, err := s.Queries.GetWorkspaceOwnerUserID(ctx, d.WorkspaceID)
	if err != nil {
		return db.Issue{}, false, fmt.Errorf("workspace owner: %w", err)
	}
	prs, err := s.Queries.ListReleaseMergedPullRequests(ctx, db.ListReleaseMergedPullRequestsParams{
		WorkspaceID: d.WorkspaceID, RepoOwner: d.RepoOwner, RepoName: d.RepoName, SourceSha: d.NewSHA,
	})
	if err != nil {
		return db.Issue{}, false, fmt.Errorf("list merged pull requests: %w", err)
	}
	res, err := s.Create(ctx, IssueCreateParams{
		WorkspaceID: d.WorkspaceID,
		Title:       fmt.Sprintf("release(%s): %s..%s", d.RepoName, short(d.OldSHA), short(d.NewSHA)),
		Description: pgtype.Text{String: releaseBody(d, prs), Valid: true},
		Status:      "todo", Priority: "none",
		CreatorType: "member", CreatorID: owner,
		AllowDuplicate: true,
	}, IssueCreateOpts{})
	if err != nil {
		// Free the claim so the next delivery retries at once, not in two minutes.
		_ = s.Queries.DeleteUnissuedGitHubReleaseDeploy(ctx, db.DeleteUnissuedGitHubReleaseDeployParams(key))
		return db.Issue{}, false, fmt.Errorf("create release issue: %w", err)
	}
	if err := s.Queries.SetGitHubReleaseDeployIssue(ctx, db.SetGitHubReleaseDeployIssueParams{
		WorkspaceID: key.WorkspaceID, RepoOwner: key.RepoOwner, RepoName: key.RepoName, SourceSha: key.SourceSha, ImageDigest: key.ImageDigest, IssueID: res.Issue.ID,
	}); err != nil {
		return db.Issue{}, false, fmt.Errorf("record release issue: %w", err)
	}
	if err := s.attachReleaseLabel(ctx, key, res.Issue.ID); err != nil {
		return db.Issue{}, false, err
	}
	return res.Issue, true, nil
}

// finishDuplicateRelease handles a deploy that is already claimed: it returns
// the existing issue, and attaches the label if the first try died before it.
func (s *IssueService) finishDuplicateRelease(ctx context.Context, d ReleaseDeploy, key db.GetGitHubReleaseDeployParams) (db.Issue, bool, error) {
	row, err := s.Queries.GetGitHubReleaseDeploy(ctx, key)
	if err != nil {
		return db.Issue{}, false, fmt.Errorf("read release deploy: %w", err)
	}
	if !row.IssueID.Valid {
		return db.Issue{}, false, nil // another delivery is mid-create
	}
	if !row.LabeledAt.Valid {
		if err := s.attachReleaseLabel(ctx, key, row.IssueID); err != nil {
			return db.Issue{}, false, err
		}
	}
	issue, err := s.Queries.GetIssue(ctx, row.IssueID)
	if err != nil {
		return db.Issue{}, false, fmt.Errorf("read release issue: %w", err)
	}
	return issue, false, nil
}

func (s *IssueService) attachReleaseLabel(ctx context.Context, key db.GetGitHubReleaseDeployParams, issueID pgtype.UUID) error {
	label, err := s.Queries.EnsureIssueLabelByName(ctx, db.EnsureIssueLabelByNameParams{
		WorkspaceID: key.WorkspaceID, Name: ReleaseLabel, Description: "Release issue", Color: "#2563eb",
	})
	if err != nil {
		return fmt.Errorf("ensure release label: %w", err)
	}
	if _, err := s.Queries.AttachLabelToIssue(ctx, db.AttachLabelToIssueParams{IssueID: issueID, LabelID: label.ID, WorkspaceID: key.WorkspaceID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("attach release label: %w", err)
	}
	return s.Queries.SetGitHubReleaseDeployLabeled(ctx, db.SetGitHubReleaseDeployLabeledParams(key))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

var conventionalTitle = regexp.MustCompile(`^(\w+)(?:\(([^)]*)\))?!?:`)

// releaseGroup buckets a PR title: a cli or skill scope wins over the type.
func releaseGroup(title string) string {
	m := conventionalTitle.FindStringSubmatch(title)
	if m == nil {
		return "other"
	}
	scope := strings.ToLower(m[2])
	switch {
	case strings.Contains(scope, "cli"):
		return "CLI"
	case strings.Contains(scope, "skill"):
		return "skill"
	case m[1] == "feat" || m[1] == "fix":
		return m[1]
	}
	return "other"
}

func releaseBody(d ReleaseDeploy, prs []db.ListReleaseMergedPullRequestsRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Deployed to C00 from `%s` to `%s`.\n\n- SHA: `%s`\n- Digest: `%s`\n- Previous SHA: `%s`\n- Run: https://github.com/%s/%s/actions/runs/%d\n\n## Merged PRs\n",
		d.OldSHA, d.NewSHA, d.NewSHA, d.Digest, d.OldSHA, d.RepoOwner, d.RepoName, d.RunID)
	groups := map[string][]string{}
	for _, pr := range prs {
		g := releaseGroup(pr.Title)
		groups[g] = append(groups[g], fmt.Sprintf("- [#%d](%s) %s", pr.PrNumber, pr.HtmlUrl, pr.Title))
	}
	for _, g := range []string{"feat", "fix", "CLI", "skill", "other"} {
		if len(groups[g]) > 0 {
			fmt.Fprintf(&b, "\n### %s\n%s\n", g, strings.Join(groups[g], "\n"))
		}
	}
	if len(prs) == 0 {
		b.WriteString("\nNone found in the mirror.\n")
	}
	return b.String()
}
