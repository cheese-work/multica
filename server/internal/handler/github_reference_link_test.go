package handler

import (
	"fmt"
	"strings"
	"testing"
)

func TestWebhook_ReferenceLinkKinds(t *testing.T) {
	if testHandler == nil {
		t.Fatal("handler test fixture not initialized")
	}
	secret := "reference-link-kind-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
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
		t.Run(scenario.name, func(t *testing.T) {
			installationID := int64(30264880 + index)
			issue := prAutoCompleteTestIssue(t, scenario.name, installationID)
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
				firePRWebhook(t, secret, installationID, 1, title, "", branch, "merged")
				if count := linkedPRCountForTest(t, issue.ID); count != 0 {
					t.Fatalf("PR linked before reference edit: %d", count)
				}
				firePRWebhook(t, secret, installationID, 1, title, body, branch, "edited_merged")
			} else {
				firePRWebhook(t, secret, installationID, 1, title, body, branch, "opened")
				firePRWebhook(t, secret, installationID, 1, title, body, branch, "merged")
			}
			if got := issueStatusForTest(t, issue.ID); got != scenario.wantStatus {
				t.Errorf("status = %q, want %q", got, scenario.wantStatus)
			}
			list := listIssuePRsForTest(t, issue.ID)
			if len(list.PullRequests) != 1 {
				t.Fatalf("linked PR count = %d, want 1", len(list.PullRequests))
			}
			if got := list.PullRequests[0].LinkSource; got != scenario.wantSource {
				t.Errorf("link_source = %q, want %q", got, scenario.wantSource)
			}
			if scenario.closing == "" && list.AutoComplete.State != prAutoCompleteNone {
				t.Errorf("auto_complete.state = %q, want %q", list.AutoComplete.State, prAutoCompleteNone)
			}
		})
	}
}

func TestWebhook_ReferencePRDoesNotBlockClosingPR(t *testing.T) {
	if testHandler == nil {
		t.Fatal("handler test fixture not initialized")
	}
	secret := "reference-does-not-block-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	const installationID int64 = 30264890
	issue := prAutoCompleteTestIssue(t, "closing plus reference", installationID)
	firePRWebhook(t, secret, installationID, 1, "reference work", "Refs "+issue.Identifier, "fix/reference", "opened")
	firePRWebhook(t, secret, installationID, 2, issue.Identifier+": delivery", "", "fix/delivery", "merged")
	if got := issueStatusForTest(t, issue.ID); got != "done" {
		t.Errorf("status = %q, want done despite open reference PR", got)
	}
	if got := linkedPRCountForTest(t, issue.ID); got != 2 {
		t.Errorf("linked PR count = %d, want 2", got)
	}
}

func TestWebhook_ReferenceEventDoesNotTriggerExistingDelivery(t *testing.T) {
	if testHandler == nil {
		t.Fatal("handler test fixture not initialized")
	}
	secret := "reference-event-only-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	const installationID int64 = 30264893
	issue := prAutoCompleteTestIssue(t, "previously delivered work", installationID)
	linkMergedGitHubPRForTest(t, issue.ID, "previous-delivery")
	for _, lifecycle := range []string{"opened", "merged", "edited_merged"} {
		firePRWebhook(t, secret, installationID, 1, "reference work", "Refs "+issue.Identifier, "fix/reference", lifecycle)
		if got := issueStatusForTest(t, issue.ID); got != "in_progress" {
			t.Errorf("%s: status = %q, want in_progress", lifecycle, got)
		}
	}
}

func TestWebhook_LinkKindEdits(t *testing.T) {
	if testHandler == nil {
		t.Fatal("handler test fixture not initialized")
	}
	secret := "reference-link-edit-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	for index, closingFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("closing_first_%t", closingFirst), func(t *testing.T) {
			installationID := int64(30264891 + index)
			issue := prAutoCompleteTestIssue(t, "link kind edit", installationID)
			title, editedTitle := "reference work", issue.Identifier+": delivery"
			wantStatus, wantSource := "done", "title"
			if closingFirst {
				title, editedTitle = editedTitle, title
				wantStatus, wantSource = "in_progress", "reference"
			}
			body := "Refs " + issue.Identifier
			firePRWebhook(t, secret, installationID, 1, title, body, "fix/link-kind-edit", "opened")
			firePRWebhook(t, secret, installationID, 1, editedTitle, body, "fix/link-kind-edit", "edited")
			firePRWebhook(t, secret, installationID, 1, editedTitle, body, "fix/link-kind-edit", "merged")
			if got := issueStatusForTest(t, issue.ID); got != wantStatus {
				t.Errorf("status = %q, want %q", got, wantStatus)
			}
			list := listIssuePRsForTest(t, issue.ID)
			if len(list.PullRequests) != 1 || list.PullRequests[0].LinkSource != wantSource {
				t.Errorf("linked PRs = %+v, want one with source %q", list.PullRequests, wantSource)
			}
		})
	}
}
