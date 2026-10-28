package ghsnapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func signedReviewRevision(event, action, body string) ReviewRevision {
	digest := sha256.Sum256([]byte(body))
	return ReviewRevision{
		Event: event, Action: action, ObjectID: 77, NodeID: "signed-node-id",
		BodyHash: hex.EncodeToString(digest[:]), AuthorID: 42, SenderID: 42,
		AuthorType: "User", SenderType: "User", ReviewState: "changes_requested",
	}
}

func TestReviewAdmissionMatrix(test *testing.T) {
	for _, scenario := range []struct {
		name, event, action, body, editor, editedAt, state, wantState, wantReason string
		senderID, trustedID                                                       int64
	}{
		{"T13_review_comment", "pull_request_review_comment", "created", "signed text", "null", "null", "", "admitted", "verified_revision", 42, 42},
		{"T13_issue_comment", "issue_comment", "created", "signed text", "null", "null", "", "admitted", "verified_revision", 42, 42},
		{"T14_comment_self_edit", "issue_comment", "edited", "signed text", `{"__typename":"User","databaseId":42}`, `"2026-10-26T08:02:00Z"`, "", "admitted", "verified_revision", 42, 42},
		{"T22_empty_review", "pull_request_review", "submitted", "", "null", "null", "CHANGES_REQUESTED", "admitted", "verified_revision", 42, 42},
		{"T22_nonempty_review", "pull_request_review", "submitted", "signed text", "null", "null", "CHANGES_REQUESTED", "admitted", "verified_revision", 42, 42},
		{"T23_review_self_edit", "pull_request_review", "edited", "signed text", `{"__typename":"User","databaseId":42}`, `"2026-10-26T08:02:00Z"`, "CHANGES_REQUESTED", "admitted", "verified_revision", 42, 42},
		{"T3_foreign_event_sender", "issue_comment", "edited", "signed text", `{"__typename":"User","databaseId":42}`, `"2026-10-26T08:02:00Z"`, "", "quarantined", "foreign_editor", 99, 42},
		{"T3_foreign_editor", "issue_comment", "created", "signed text", `{"__typename":"User","databaseId":99}`, `"2026-10-26T08:02:00Z"`, "", "quarantined", "foreign_editor", 42, 42},
		{"T4_content_drift", "issue_comment", "created", "changed text", "null", "null", "", "quarantined", "body_changed", 42, 42},
		{"T15_null_editor", "issue_comment", "edited", "signed text", "null", `"2026-10-26T08:02:00Z"`, "", "quarantined", "unknown_edit_provenance", 42, 42},
		{"T15_bot_editor", "issue_comment", "edited", "signed text", `{"__typename":"Bot","databaseId":42}`, `"2026-10-26T08:02:00Z"`, "", "quarantined", "unknown_edit_provenance", 42, 42},
		{"T24_review_state_drift", "pull_request_review", "submitted", "signed text", "null", "null", "APPROVED", "quarantined", "review_state_changed", 42, 42},
		{"T24_review_body_drift", "pull_request_review", "submitted", "changed text", "null", "null", "CHANGES_REQUESTED", "quarantined", "body_changed", 42, 42},
		{"T24_review_foreign_editor", "pull_request_review", "submitted", "signed text", `{"__typename":"User","databaseId":99}`, `"2026-10-26T08:02:00Z"`, "CHANGES_REQUESTED", "quarantined", "foreign_editor", 42, 42},
		{"initial_edited_since_event", "issue_comment", "created", "signed text", `{"__typename":"User","databaseId":42}`, `"2026-10-26T08:02:00Z"`, "", "quarantined", "initial_revision_edited", 42, 42},
		{"self_edit_without_editor", "issue_comment", "edited", "signed text", "null", "null", "", "quarantined", "unknown_edit_provenance", 42, 42},
		{"unmapped", "issue_comment", "created", "signed text", "null", "null", "", "denied", "untrusted_author", 42, 0},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			revision := signedReviewRevision(scenario.event, scenario.action, "signed text")
			if scenario.name == "T22_empty_review" {
				revision = signedReviewRevision(scenario.event, scenario.action, "")
				revision.ObjectID = 5648946990
			}
			revision.SenderID = scenario.senderID
			kind := map[string]string{"issue_comment": "IssueComment", "pull_request_review_comment": "PullRequestReviewComment", "pull_request_review": "PullRequestReview"}[scenario.event]
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if request.Method != http.MethodPost || request.URL.Path != "/graphql" || request.Header.Get("Authorization") != "Bearer test-token" {
					test.Errorf("unexpected GraphQL request: %s %s", request.Method, request.URL.Path)
				}
				var query struct {
					Query     string            `json:"query"`
					Variables map[string]string `json:"variables"`
				}
				if err := json.NewDecoder(request.Body).Decode(&query); err != nil || query.Variables["id"] != revision.NodeID || !strings.Contains(query.Query, "lastEditedAt") || !strings.Contains(query.Query, "editor") || !strings.Contains(query.Query, "fullDatabaseId") {
					test.Errorf("query did not request signed node provenance: %+v, %v", query, err)
				}
				body, _ := json.Marshal(scenario.body)
				fmt.Fprintf(writer, `{"data":{"node":{"id":"signed-node-id","databaseId":"%d","__typename":%q,"body":%s,"editor":%s,"lastEditedAt":%s,"state":%q}}}`, revision.ObjectID, kind, body, scenario.editor, scenario.editedAt, scenario.state)
			}))
			defer server.Close()
			client := newTestClient(test, server.URL)
			client.tokens[1] = cachedToken{token: "test-token", expiry: client.now().Add(2 * tokenRenewSkew)}
			decision := client.CheckReviewRevision(context.Background(), 1, revision, scenario.trustedID)
			if decision.State != scenario.wantState || decision.Reason != scenario.wantReason {
				test.Fatalf("admission = %+v, want %s/%s", decision, scenario.wantState, scenario.wantReason)
			}
			if decision.State == "admitted" && calls != 1 {
				test.Fatalf("admission must recheck GraphQL exactly once: calls=%d", calls)
			}
		})
	}
}

func TestReviewAdmissionFailsClosed(test *testing.T) {
	for _, response := range []string{
		`{"errors":[{"message":"sensitive text"}],"data":{"node":null}}`,
		`{"data":{"node":null}}`, `{"data":{"node":{"id":"signed-node-id"}}}`, `not json`,
		`{"data":{"node":{"id":"other","databaseId":77,"__typename":"IssueComment","body":"signed text","editor":null,"lastEditedAt":null}}}`,
		`{"data":{"node":{"id":"signed-node-id","databaseId":78,"__typename":"IssueComment","body":"signed text","editor":null,"lastEditedAt":null}}}`,
		`{"data":{"node":{"id":"signed-node-id","databaseId":77,"__typename":"PullRequestReviewComment","body":"signed text","editor":null,"lastEditedAt":null}}}`,
		`{"data":{"node":{"id":"signed-node-id","databaseId":77,"__typename":"IssueComment","body":"signed text","editor":null}}}`,
	} {
		test.Run(response, func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { fmt.Fprint(writer, response) }))
			defer server.Close()
			client := newTestClient(test, server.URL)
			client.tokens[1] = cachedToken{token: "test-token", expiry: client.now().Add(2 * tokenRenewSkew)}
			decision := client.CheckReviewRevision(context.Background(), 1, signedReviewRevision("issue_comment", "created", "signed text"), 42)
			if decision.State != "deferred" || decision.Reason != "provenance_lookup_failed" {
				test.Fatalf("malformed or wrong node admitted: %+v", decision)
			}
		})
	}
}

func TestReviewAdmissionWithdrawalAndUnknownPairs(test *testing.T) {
	for _, pair := range [][2]string{
		{"pull_request_review", "dismissed"}, {"pull_request_review_comment", "deleted"}, {"issue_comment", "deleted"},
		{"pull_request_review", "created"}, {"pull_request_review_comment", "submitted"}, {"issue_comment", "submitted"}, {"other", "created"},
	} {
		decision := (*Client)(nil).CheckReviewRevision(context.Background(), 1, signedReviewRevision(pair[0], pair[1], "signed text"), 42)
		if decision.State != "denied" {
			test.Errorf("%s/%s = %+v, want denied without lookup", pair[0], pair[1], decision)
		}
	}
}

func TestReviewAdmissionTransportFailures(test *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		test.Run(fmt.Sprint(status), func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(status)
				fmt.Fprint(writer, "untrusted remote error body")
			}))
			defer server.Close()
			client := newTestClient(test, server.URL)
			client.tokens[1] = cachedToken{token: "test-token", expiry: client.now().Add(2 * tokenRenewSkew)}
			decision := client.CheckReviewRevision(context.Background(), 1, signedReviewRevision("issue_comment", "created", "signed text"), 42)
			if decision != (ReviewAdmission{"deferred", "provenance_lookup_failed"}) {
				test.Fatalf("API error = %+v, want deferred without remote text", decision)
			}
		})
	}
	client := newTestClient(test, "http://127.0.0.1:1")
	client.tokens[1] = cachedToken{token: "test-token", expiry: client.now().Add(2 * tokenRenewSkew)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if decision := client.CheckReviewRevision(ctx, 1, signedReviewRevision("issue_comment", "created", "signed text"), 42); decision.State != "deferred" {
		test.Fatalf("cancelled lookup = %+v", decision)
	}
}
