package dmnotify

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

type replyRow func(...any) error

func (row replyRow) Scan(destinations ...any) error { return row(destinations...) }

type replyDB struct {
	db.DBTX
	preferences     string
	preferenceError error
	inApp           bool
	push            bool
	writes          int
}

func (database *replyDB) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	return replyRow(func(destinations ...any) error {
		switch {
		case strings.Contains(query, "-- name: GetChatSession"):
			*destinations[0].(*pgtype.UUID) = dbid.NewV7()
			*destinations[1].(*pgtype.UUID) = dbid.NewV7()
			*destinations[3].(*pgtype.UUID) = dbid.NewV7()
			*destinations[7].(*string) = "active"
		case strings.Contains(query, "-- name: ChatSessionHasCurrentChannelBinding"):
			*destinations[0].(*bool) = false
		case strings.Contains(query, "-- name: GetDMChannelSwitches"):
			*destinations[0].(*bool) = database.inApp
			*destinations[1].(*bool) = database.push
		case strings.Contains(query, "-- name: GetNotificationPreference"):
			if database.preferenceError != nil {
				return database.preferenceError
			}
			if database.preferences == "" {
				return pgx.ErrNoRows
			}
			*destinations[3].(*[]byte) = []byte(database.preferences)
		case strings.Contains(query, "-- name: UpsertDMReplyInboxItem"):
			database.writes++
			*destinations[14].(*[]byte) = []byte(`{"reply_count":1}`)
		}
		return nil
	})
}

func TestReplyDeliveryScope(test *testing.T) {
	for _, scenario := range []struct {
		name, preferences    string
		inApp, push, allowed bool
	}{
		{name: "default with in-app channel", inApp: true, allowed: true},
		{name: "default with push channel", push: true, allowed: true},
		{name: "explicit all", preferences: `{"unassigned_activity":"all"}`, inApp: true, allowed: true},
		{name: "restricted in-app DM", preferences: `{"unassigned_activity":"muted"}`, inApp: true},
		{name: "restricted push DM", preferences: `{"unassigned_activity":"muted"}`, push: true},
		{name: "channels still disabled"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			database := &replyDB{preferences: scenario.preferences, inApp: scenario.inApp, push: scenario.push}
			notified, err := Reply(context.Background(), db.New(database), dbid.NewV7(), db.ChatMessage{Role: "assistant"})
			if err != nil {
				test.Fatal(err)
			}
			if (notified != nil) != scenario.allowed {
				test.Fatalf("notified=%v, want delivery=%v", notified, scenario.allowed)
			}
			if !scenario.allowed && database.writes != 0 {
				test.Fatal("suppressed reply mutated the coalesced inbox record")
			}
		})
	}
}

func TestReplyDeliveryPreferenceFailure(test *testing.T) {
	failure := errors.New("preferences unavailable")
	database := &replyDB{preferenceError: failure, inApp: true}
	notified, err := Reply(context.Background(), db.New(database), dbid.NewV7(), db.ChatMessage{Role: "assistant"})
	if notified != nil || !errors.Is(err, failure) || database.writes != 0 {
		test.Fatalf("notified=%v, err=%v, writes=%d", notified, err, database.writes)
	}
}
