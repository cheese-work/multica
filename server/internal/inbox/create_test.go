package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

type deliveryRow func(...any) error

func (row deliveryRow) Scan(destinations ...any) error { return row(destinations...) }

type deliveryDB struct {
	db.DBTX
	preferences     []byte
	preferenceError error
	assigneeType    string
	assigneeID      pgtype.UUID
	creatorID       pgtype.UUID
	issueError      error
	queries         []string
}

func (database *deliveryDB) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	database.queries = append(database.queries, query)
	return deliveryRow(func(destinations ...any) error {
		switch {
		case strings.Contains(query, "-- name: GetNotificationPreference"):
			if database.preferenceError != nil {
				return database.preferenceError
			}
			if database.preferences == nil {
				return pgx.ErrNoRows
			}
			*destinations[3].(*[]byte) = database.preferences
		case strings.Contains(query, "-- name: GetIssueInWorkspace"):
			if database.issueError != nil {
				return database.issueError
			}
			*destinations[6].(*pgtype.Text) = pgtype.Text{String: database.assigneeType, Valid: database.assigneeType != ""}
			*destinations[7].(*pgtype.UUID) = database.assigneeID
			*destinations[8].(*string) = "member"
			*destinations[9].(*pgtype.UUID) = database.creatorID
		}
		return nil
	})
}

func TestCreateIfEligibleDeliveryScope(test *testing.T) {
	recipientID := dbid.NewV7()
	otherID := dbid.NewV7()
	for _, scenario := range []struct {
		name          string
		preferences   string
		assigneeType  string
		assigneeID    pgtype.UUID
		details       string
		issue         bool
		recipientType string
		allowed       bool
	}{
		{name: "default watcher", issue: true, allowed: true},
		{name: "explicit all watcher", preferences: `{"unassigned_activity":"all"}`, issue: true, allowed: true},
		{name: "unrelated mute preserved", preferences: `{"comments":"muted"}`, issue: true, allowed: true},
		{name: "member assignee", assigneeType: "member", assigneeID: recipientID, issue: true, allowed: true},
		{name: "different member", assigneeType: "member", assigneeID: otherID, issue: true},
		{name: "agent is not member", assigneeType: "agent", assigneeID: recipientID, issue: true},
		{name: "squad is not member", assigneeType: "squad", assigneeID: recipientID, issue: true},
		{name: "watcher excluded", issue: true},
		{name: "creator excluded", issue: true},
		{name: "ordinary DM excluded"},
		{name: "decision without assignment", details: `{"decision_request_id":"` + dbid.NewV7().String() + `"}`, issue: true, allowed: true},
		{name: "decision despite mention mute", preferences: `{"unassigned_activity":"muted","mentions":"muted"}`, details: `{"decision_request_id":"` + dbid.NewV7().String() + `"}`, issue: true, allowed: true},
		{name: "decision expiry digest", details: `{"kind":"decision_expiry_digest","decision_request_ids":["` + dbid.NewV7().String() + `"]}`, allowed: true},
		{name: "empty decision expiry digest", details: `{"kind":"decision_expiry_digest","decision_request_ids":[]}`},
		{name: "empty decision marker excluded", details: `{"decision_request_id":""}`, issue: true},
		{name: "null decision marker excluded", details: `{"decision_request_id":null}`, issue: true},
		{name: "agent recipient unchanged", recipientType: "agent", issue: true, allowed: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			preferences := scenario.preferences
			if preferences == "" && scenario.name != "default watcher" {
				preferences = `{"unassigned_activity":"muted"}`
			}
			database := &deliveryDB{assigneeType: scenario.assigneeType, assigneeID: scenario.assigneeID}
			if scenario.name == "creator excluded" {
				database.creatorID = recipientID
			}
			if preferences != "" {
				database.preferences = []byte(preferences)
			}
			params := db.CreateInboxItemParams{WorkspaceID: dbid.NewV7(), RecipientType: "member", RecipientID: recipientID, Type: "mentioned", Details: []byte(scenario.details)}
			if scenario.issue {
				params.IssueID = dbid.NewV7()
			}
			if scenario.recipientType != "" {
				params.RecipientType = scenario.recipientType
			}
			_, inserted, err := CreateIfEligible(context.Background(), db.New(database), params)
			if err != nil {
				test.Fatal(err)
			}
			if inserted != scenario.allowed {
				test.Fatalf("inserted=%v, want %v", inserted, scenario.allowed)
			}
		})
	}
}

func TestCreateIfEligibleDeliveryScopeErrors(test *testing.T) {
	failure := errors.New("database unavailable")
	for _, database := range []*deliveryDB{
		{preferenceError: failure},
		{preferences: []byte(`{"unassigned_activity":"muted"}`), issueError: failure},
	} {
		_, inserted, err := CreateIfEligible(context.Background(), db.New(database), db.CreateInboxItemParams{
			WorkspaceID: dbid.NewV7(), RecipientType: "member", RecipientID: dbid.NewV7(), IssueID: dbid.NewV7(),
		})
		if !errors.Is(err, failure) || inserted {
			test.Fatalf("inserted=%v, err=%v; want database error and no delivery", inserted, err)
		}
	}
}
