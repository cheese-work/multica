package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// releaseIssueID is the issue id of one deploy: a v5 UUID of its identity
// (workspace, repo, SHA, digest). The issue primary key is the dedup and the
// recovery key, so the create is atomic by itself: no claim row has to agree
// with it across two transactions.
func releaseIssueID(d ReleaseDeploy) pgtype.UUID {
	name := fmt.Sprintf("release-deploy:%x:%s/%s:%s:%s", d.WorkspaceID.Bytes, d.RepoOwner, d.RepoName, d.NewSHA, d.Digest)
	return pgtype.UUID{Bytes: uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)), Valid: true}
}

// CreateRelease opens the release issue for a deploy exactly once. The issue
// is created under a deterministic id; a redelivery, a rerun or a concurrent
// caller hits the primary key and creates nothing. Every call then finishes the
// idempotent steps an earlier try may have left undone: the link row and the
// label write. created is true only for the call that made the issue.
func (s *IssueService) CreateRelease(ctx context.Context, d ReleaseDeploy) (issue db.Issue, created bool, err error) {
	id := releaseIssueID(d)
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
		ID:          id,
		WorkspaceID: d.WorkspaceID,
		Title:       fmt.Sprintf("release(%s): %s..%s", d.RepoName, short(d.OldSHA), short(d.NewSHA)),
		Description: pgtype.Text{String: releaseBody(d, prs), Valid: true},
		Status:      "todo", Priority: "none",
		CreatorType: "member", CreatorID: owner,
		AllowDuplicate: true,
	}, IssueCreateOpts{})
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		issue, created = res.Issue, true
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "issue_pkey":
		// The issue already exists: an earlier try or a concurrent caller made it.
		if issue, err = s.Queries.GetIssue(ctx, id); err != nil {
			return db.Issue{}, false, fmt.Errorf("read release issue: %w", err)
		}
	default:
		return db.Issue{}, false, fmt.Errorf("create release issue: %w", err)
	}
	if err := s.Queries.LinkGitHubReleaseDeploy(ctx, db.LinkGitHubReleaseDeployParams{
		WorkspaceID: d.WorkspaceID, RepoOwner: d.RepoOwner, RepoName: d.RepoName,
		SourceSha: d.NewSHA, ImageDigest: d.Digest, OldSha: d.OldSHA, RunID: d.RunID, IssueID: id,
	}); err != nil {
		return db.Issue{}, false, fmt.Errorf("record release issue: %w", err)
	}
	key := db.GetGitHubReleaseDeployParams{WorkspaceID: d.WorkspaceID, RepoOwner: d.RepoOwner, RepoName: d.RepoName, SourceSha: d.NewSHA, ImageDigest: d.Digest}
	if err := s.attachReleaseLabel(ctx, key, id); err != nil {
		return db.Issue{}, false, err
	}
	return issue, created, nil
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
