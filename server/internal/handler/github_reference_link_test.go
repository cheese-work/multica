package handler

import (
	"fmt"
	"strings"
	"testing"
)

func TestWebhook_ReferenceLinkKinds(test *testing.T) {
	if testHandler == nil {
		test.Fatal("handler test fixture not initialized")
	}
	secret := "reference-link-kind-secret"
	test.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	for index, scenario := range []struct {
		name       string
		keyword    string
		closing    string
		postMerge  bool
		wantStatus string
		wantSource string
	}{
		{name: "refs_only", keyword: "Refs", wantStatus: "in_progress", wantSource: "reference"},
		{name: "part_of_only", keyword: "Part of", wantStatus: "in_progress", wantSource: "reference"},
		{name: "title_and_refs", keyword: "Refs", closing: "title", wantStatus: "done", wantSource: "title"},
		{name: "branch_and_part_of", keyword: "Part of", closing: "branch", wantStatus: "done", wantSource: "branch"},
		{name: "closes_and_refs", keyword: "Refs", closing: "body", wantStatus: "done", wantSource: "auto"},
		{name: "post_merge_part_of", keyword: "Part of", postMerge: true, wantStatus: "in_progress", wantSource: "reference"},
		{name: "post_merge_refs", keyword: "Refs", postMerge: true, wantStatus: "in_progress", wantSource: "reference"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			installationID := int64(30264880 + index)
			issue := prAutoCompleteTestIssue(test, scenario.name, installationID)
			title, branch := "reference work", "fix/reference-work"
			body := fmt.Sprintf("%s %s", scenario.keyword, issue.Identifier)
			switch scenario.closing {
			case "title":
				title = issue.Identifier + ": delivered work"
			case "branch":
				branch = "fix/" + strings.ToLower(issue.Identifier)
			case "body":
				body += "\nCloses " + issue.Identifier
			}
			if scenario.postMerge {
				firePRWebhook(test, secret, installationID, 1, title, "", branch, "merged")
				if count := linkedPRCountForTest(test, issue.ID); count != 0 {
					test.Fatalf("PR linked before reference edit: %d", count)
				}
				firePRWebhook(test, secret, installationID, 1, title, body, branch, "edited_merged")
			} else {
				firePRWebhook(test, secret, installationID, 1, title, body, branch, "opened")
				firePRWebhook(test, secret, installationID, 1, title, body, branch, "merged")
			}
			if got := issueStatusForTest(test, issue.ID); got != scenario.wantStatus {
				test.Errorf("status = %q, want %q", got, scenario.wantStatus)
			}
			list := listIssuePRsForTest(test, issue.ID)
			if len(list.PullRequests) != 1 {
				test.Fatalf("linked PR count = %d, want 1", len(list.PullRequests))
			}
			if got := list.PullRequests[0].LinkSource; got != scenario.wantSource {
				test.Errorf("link_source = %q, want %q", got, scenario.wantSource)
			}
			if scenario.closing == "" && list.AutoComplete.State != prAutoCompleteNone {
				test.Errorf("auto_complete.state = %q, want %q", list.AutoComplete.State, prAutoCompleteNone)
			}
		})
	}
}

func TestWebhook_ReferencePRDoesNotBlockClosingPR(test *testing.T) {
	if testHandler == nil {
		test.Fatal("handler test fixture not initialized")
	}
	secret := "reference-does-not-block-secret"
	test.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	const installationID int64 = 30264890
	issue := prAutoCompleteTestIssue(test, "closing plus reference", installationID)
	firePRWebhook(test, secret, installationID, 1, "reference work", "Refs "+issue.Identifier, "fix/reference", "opened")
	firePRWebhook(test, secret, installationID, 2, issue.Identifier+": delivery", "", "fix/delivery", "merged")
	if got := issueStatusForTest(test, issue.ID); got != "done" {
		test.Errorf("status = %q, want done despite open reference PR", got)
	}
	if got := linkedPRCountForTest(test, issue.ID); got != 2 {
		test.Errorf("linked PR count = %d, want 2", got)
	}
}

func TestWebhook_ReferenceEventDoesNotTriggerExistingDelivery(test *testing.T) {
	if testHandler == nil {
		test.Fatal("handler test fixture not initialized")
	}
	secret := "reference-event-only-secret"
	test.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	const installationID int64 = 30264893
	issue := prAutoCompleteTestIssue(test, "previously delivered work", installationID)
	linkMergedGitHubPRForTest(test, issue.ID, "previous-delivery")
	for _, lifecycle := range []string{"opened", "merged", "edited_merged"} {
		firePRWebhook(test, secret, installationID, 1, "reference work", "Refs "+issue.Identifier, "fix/reference", lifecycle)
		if got := issueStatusForTest(test, issue.ID); got != "in_progress" {
			test.Errorf("%s: status = %q, want in_progress", lifecycle, got)
		}
	}
}

func TestWebhook_LinkKindEdits(test *testing.T) {
	if testHandler == nil {
		test.Fatal("handler test fixture not initialized")
	}
	secret := "reference-link-edit-secret"
	test.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	for index, closingFirst := range []bool{false, true} {
		test.Run(fmt.Sprintf("closing_first_%t", closingFirst), func(test *testing.T) {
			installationID := int64(30264891 + index)
			issue := prAutoCompleteTestIssue(test, "link kind edit", installationID)
			title, editedTitle := "reference work", issue.Identifier+": delivery"
			wantStatus, wantSource := "done", "title"
			if closingFirst {
				title, editedTitle = editedTitle, title
				wantStatus, wantSource = "in_progress", "reference"
			}
			body := "Refs " + issue.Identifier
			firePRWebhook(test, secret, installationID, 1, title, body, "fix/link-kind-edit", "opened")
			firePRWebhook(test, secret, installationID, 1, editedTitle, body, "fix/link-kind-edit", "edited")
			firePRWebhook(test, secret, installationID, 1, editedTitle, body, "fix/link-kind-edit", "merged")
			if got := issueStatusForTest(test, issue.ID); got != wantStatus {
				test.Errorf("status = %q, want %q", got, wantStatus)
			}
			list := listIssuePRsForTest(test, issue.ID)
			if len(list.PullRequests) != 1 || list.PullRequests[0].LinkSource != wantSource {
				test.Errorf("linked PRs = %+v, want one with source %q", list.PullRequests, wantSource)
			}
		})
	}
}
