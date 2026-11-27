package service

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/reviewindependence"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// defaultAttribute names LabOf's value while the review_independence setting is
// absent.
const defaultAttribute = "lab"

// ReviewValues is the one resolver of the review-independence rule (CHE-9319):
// reviewer routing and the merge gate both ask it for the value of an agent and
// of a PR's commits. With the workspace setting present the values come from the
// setting; with it absent they come from LabOf, as before.
type ReviewValues struct {
	cfg reviewindependence.Config
	set bool
}

// ReviewValuesFromSettings reads the resolver from a workspace settings column.
func ReviewValuesFromSettings(settings []byte) ReviewValues {
	cfg, ok := reviewindependence.FromSettingsJSON(settings)
	return ReviewValues{cfg: cfg, set: ok}
}

// ReviewValuesForWorkspace reads the resolver inside a transaction.
func ReviewValuesForWorkspace(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID) (ReviewValues, error) {
	var settings []byte
	err := tx.QueryRow(ctx, `SELECT settings FROM workspace WHERE id=$1`, workspaceID).Scan(&settings)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewValues{}, nil
	}
	if err != nil {
		return ReviewValues{}, err
	}
	return ReviewValuesFromSettings(settings), nil
}

// Configured reports whether the review_independence setting is present.
func (r ReviewValues) Configured() bool { return r.set }

// Attribute is the name of the compared attribute, for refusals.
func (r ReviewValues) Attribute() string {
	if r.set {
		return r.cfg.Attribute
	}
	return defaultAttribute
}

// AgentValue is the value of the agent behind a runtime provider and model, ""
// when it has none: an agent without a value is neither a reviewer nor an author
// the rule accepts.
func (r ReviewValues) AgentValue(provider, model string) string {
	if r.set {
		return r.cfg.AgentValue(provider, model)
	}
	return LabOf(model, provider)
}

// CommitAuthorValues maps each value named on the commits to the commits that
// name it, as "commit <sha12>". ok is false when the setting is absent, there is
// no commit, or any commit names no mapped identity: the commits then cannot
// show who wrote the code and the caller keeps the run-based rule.
func (r ReviewValues) CommitAuthorValues(commits []reviewindependence.PRCommit) (values map[string][]string, ok bool) {
	if !r.set || len(commits) == 0 {
		return nil, false
	}
	values = map[string][]string{}
	for _, c := range commits {
		named := r.cfg.CommitValues(c)
		if len(named) == 0 {
			return nil, false
		}
		sha := c.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		for _, v := range named {
			values[v] = append(values[v], "commit "+sha)
		}
	}
	return values, true
}

// AuthorSet is who wrote an issue's work, as the rule sees it: the values they
// hold, the commit or run behind each value, and the agents among them.
type AuthorSet struct {
	Values   []string            // sorted
	Evidence map[string][]string // value -> "commit <sha12>" or "run <id8>"
	AgentIDs map[string]bool     // run-based authors only
}

// IssueAuthorSet is the one author computation, used by reviewer routing and the
// merge gate (CHE-9319). With the setting present and the PRs' commits known
// (commits non-nil), the authors are the values on the commits: an agent that
// only moved statuses or commented is not one. Otherwise every run at a work
// step counts, its agent valued by this resolver.
func (r ReviewValues) IssueAuthorSet(ctx context.Context, q *db.Queries, issueID pgtype.UUID, commits []reviewindependence.PRCommit) (AuthorSet, error) {
	set := AuthorSet{Evidence: map[string][]string{}, AgentIDs: map[string]bool{}}
	add := func(value, evidence string) {
		if !slices.Contains(set.Values, value) {
			set.Values = append(set.Values, value)
		}
		if !slices.Contains(set.Evidence[value], evidence) {
			set.Evidence[value] = append(set.Evidence[value], evidence)
		}
	}
	if values, ok := r.CommitAuthorValues(commits); ok {
		for value, evidence := range values {
			for _, e := range evidence {
				add(value, e)
			}
		}
		slices.Sort(set.Values)
		return set, nil
	}
	authors, err := IssueAuthors(ctx, q, issueID, r.AgentValue)
	if err != nil {
		return AuthorSet{}, err
	}
	for _, a := range authors {
		set.AgentIDs[a.AgentID] = true
		add(a.Lab, "run "+shortID(a.TaskID))
	}
	slices.Sort(set.Values)
	return set, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
