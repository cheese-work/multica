package handler

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// CHE-6619 (CHE-6594 S1a): Vote lines come from vote_lines, Route and Risk
// lines from full_body of the same governed_vote_record row. Live comment
// text is never read.

const (
	cfgLabel  = "agent.instructions:00000000-0000-4000-8000-000000000001"
	cfgAfter  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfgBefore = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func cfgReviewBody(route, risk string) string {
	return "Reviewed the diff.\n" +
		"Vote: PASS " + cfgLabel + " " + cfgAfter + " from " + cfgBefore + "\n" +
		"Route: " + route + " " + cfgLabel + " " + cfgAfter + "\n" +
		"Risk: " + risk + "\n"
}

func cfgIssue(t *testing.T) string {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	return dbfx.Issue(t, "CHE-6619 receipt "+safeTestName(t))
}

// 1. Route and Risk are parsed from full_body of the reviewer's record.
func TestConfigReviewReceipt_ParsesRouteAndRiskFromFullBody(t *testing.T) {
	issue := cfgIssue(t)
	id := dbfx.Comment(t, issue, cfgReviewBody("apply", "Touches one agent's instructions only."))

	got, err := loadConfigReviewReceipt(context.Background(), dbfx.Pool, testWorkspaceID, issue, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := configReviewReceipt{
		CommentID: id,
		Votes:     []configReceiptVote{{Verdict: "PASS", Label: cfgLabel, After: cfgAfter, Before: cfgBefore, Raw: "Vote: PASS " + cfgLabel + " " + cfgAfter + " from " + cfgBefore}},
		Routes:    []configReceiptRoute{{Mode: "apply", Label: cfgLabel, After: cfgAfter, Raw: "Route: apply " + cfgLabel + " " + cfgAfter}},
		Risks:     []string{"Touches one agent's instructions only."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt = %+v, want %+v", got, want)
	}
}

// Pure parser: the same trimming and CRLF handling as the migration 809 trigger.
func TestConfigReviewReceipt_ParserLineRules(t *testing.T) {
	voteLines := "Vote: PASS " + cfgLabel + " " + cfgAfter + " from " + cfgBefore
	body := "intro\r\n  Route: cheese " + cfgLabel + " " + cfgAfter + "  \r\nRisk:  one sentence.  \r\nnot a Route: line\r\n"
	got, err := parseConfigReviewReceipt(voteLines, body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Routes) != 1 || got.Routes[0].Mode != "cheese" || got.Routes[0].Label != cfgLabel || got.Routes[0].After != cfgAfter {
		t.Fatalf("routes = %+v", got.Routes)
	}
	if !reflect.DeepEqual(got.Risks, []string{"one sentence."}) {
		t.Fatalf("risks = %q", got.Risks)
	}
}

// 2. An edit or a delete of the comment after insert changes no parse result.
func TestConfigReviewReceipt_EditAndDeleteChangeNothing(t *testing.T) {
	issue := cfgIssue(t)
	id := dbfx.Comment(t, issue, cfgReviewBody("apply", "Original risk."))
	load := func() configReviewReceipt {
		t.Helper()
		r, err := loadConfigReviewReceipt(context.Background(), dbfx.Pool, testWorkspaceID, issue, id)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return r
	}
	before := load()
	if len(before.Routes) != 1 || len(before.Risks) != 1 {
		t.Fatalf("setup: receipt = %+v", before)
	}

	dbfx.Exec(t, `UPDATE comment SET content = $1 WHERE id = $2`, cfgReviewBody("cheese", "Forged risk."), id)
	if after := load(); !reflect.DeepEqual(after, before) {
		t.Fatalf("after edit: %+v, want %+v", after, before)
	}

	dbfx.Exec(t, `DELETE FROM comment WHERE id = $1`, id)
	if after := load(); !reflect.DeepEqual(after, before) {
		t.Fatalf("after delete: %+v, want %+v", after, before)
	}
}

// 3. A record with an empty full_body is not a valid receipt.
func TestConfigReviewReceipt_EmptyFullBodyIsNotAReceipt(t *testing.T) {
	issue := cfgIssue(t)
	var id string
	dbfx.QueryRow(t, `SELECT gen_random_uuid()::text`).Scan(&id)
	dbfx.Exec(t, `INSERT INTO governed_vote_record
		(issue_id, comment_id, workspace_id, author_type, author_id, vote_lines, created_at, full_body)
		VALUES ($1, $2, $3, 'member', $4, $5, now(), '')`,
		issue, id, testWorkspaceID, testUserID, "Vote: PASS "+cfgLabel+" "+cfgAfter+" from "+cfgBefore)
	dbfx.Cleanup(t, `DELETE FROM governed_vote_record WHERE comment_id = $1`, id)

	if _, err := loadConfigReviewReceipt(context.Background(), dbfx.Pool, testWorkspaceID, issue, id); !errors.Is(err, errConfigReceiptEmptyBody) {
		t.Fatalf("load error = %v, want errConfigReceiptEmptyBody", err)
	}
	if _, err := parseConfigReviewReceipt("Vote: PASS x", ""); !errors.Is(err, errConfigReceiptEmptyBody) {
		t.Fatalf("parse error = %v, want errConfigReceiptEmptyBody", err)
	}
}

// 4. A Route line in another comment does not count for this record.
func TestConfigReviewReceipt_RouteInAnotherCommentDoesNotCount(t *testing.T) {
	issue := cfgIssue(t)
	vote := "Vote: PASS " + cfgLabel + " " + cfgAfter + " from " + cfgBefore
	bare := dbfx.Comment(t, issue, "Reviewed.\n"+vote+"\nRisk: Only the vote comment.\n")
	other := dbfx.Comment(t, issue, cfgReviewBody("apply", "Another comment's risk."))

	got, err := loadConfigReviewReceipt(context.Background(), dbfx.Pool, testWorkspaceID, issue, bare)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Routes) != 0 {
		t.Fatalf("Route leaked from comment %s: %+v", other, got.Routes)
	}
	if !reflect.DeepEqual(got.Risks, []string{"Only the vote comment."}) {
		t.Fatalf("risks = %q", got.Risks)
	}
	// A comment of another issue is not this record either.
	if _, err := loadConfigReviewReceipt(context.Background(), dbfx.Pool, testWorkspaceID, cfgIssue2(t), other); !errors.Is(err, errConfigReceiptNoRecord) {
		t.Fatalf("other issue error = %v, want errConfigReceiptNoRecord", err)
	}
}

func cfgIssue2(t *testing.T) string {
	t.Helper()
	return dbfx.Issue(t, "CHE-6619 receipt other "+safeTestName(t))
}

// Review P1 (PR 1773): every recognized Vote, Route and Risk line is kept,
// malformed or empty ones included, so S1b can enforce exactly one of each.
func TestConfigReviewReceipt_KeepsMalformedDuplicates(t *testing.T) {
	goodVote := "Vote: PASS " + cfgLabel + " " + cfgAfter + " from " + cfgBefore
	goodRoute := "Route: apply " + cfgLabel + " " + cfgAfter
	cases := []struct {
		name                 string
		voteLines, body      string
		votes, routes, risks int
	}{
		{"vote", goodVote + "\nVote: PASS", goodRoute + "\nRisk: ok.\n" + goodVote, 2, 1, 1},
		{"route", goodVote, goodRoute + "\nRoute: apply\nRisk: ok.", 1, 2, 1},
		{"risk", goodVote, goodRoute + "\nRisk: ok.\nRisk:", 1, 1, 2},
		{"risk blank", goodVote, goodRoute + "\nRisk: ok.\nRisk:    ", 1, 1, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseConfigReviewReceipt(c.voteLines, c.body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got.Votes) != c.votes || len(got.Routes) != c.routes || len(got.Risks) != c.risks {
				t.Fatalf("votes/routes/risks = %d/%d/%d, want %d/%d/%d: %+v", len(got.Votes), len(got.Routes), len(got.Risks), c.votes, c.routes, c.risks, got)
			}
			// The valid line stays well formed; the duplicate is flagged or empty.
			switch c.name {
			case "vote":
				if got.Votes[0].Malformed || !got.Votes[1].Malformed || got.Votes[1].Raw != "Vote: PASS" {
					t.Fatalf("votes = %+v", got.Votes)
				}
			case "route":
				if got.Routes[0].Malformed || !got.Routes[1].Malformed || got.Routes[1].Raw != "Route: apply" {
					t.Fatalf("routes = %+v", got.Routes)
				}
			default:
				if got.Risks[0] != "ok." || got.Risks[1] != "" {
					t.Fatalf("risks = %q", got.Risks)
				}
			}
		})
	}
}
