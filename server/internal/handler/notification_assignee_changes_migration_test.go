package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

// TestNotificationAssigneeChangesMigration runs migration 999y up and down
// against four preference shapes (CHE-6400). Up copies a muted `assignments`
// switch to the new `assignee_changes` key so nobody's delivery changes; down
// removes only the copies it could have made. Both run in a rolled-back
// transaction.
func TestNotificationAssigneeChangesMigration(t *testing.T) {
	if testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	seeded := []string{
		`{"assignments":"muted"}`,
		`{"assignments":"muted","comments":"muted"}`,
		`{}`,
		`{"assignments":"muted","assignee_changes":"all"}`,
	}
	afterUp := []string{
		`{"assignments":"muted","assignee_changes":"muted"}`,
		`{"assignments":"muted","comments":"muted","assignee_changes":"muted"}`,
		`{}`,
		`{"assignments":"muted","assignee_changes":"all"}`,
	}
	afterDown := []string{seeded[0], seeded[1], seeded[2], seeded[3]}

	up, err := os.ReadFile("../../migrations/999y_notification_assignee_changes.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/999y_notification_assignee_changes.down.sql")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	ids := make([]string, len(seeded))
	for i, prefs := range seeded {
		var userID string
		if err := tx.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
			fmt.Sprintf("Assignee changes migration %d", i), fmt.Sprintf("assignee-changes-migration-%d@example.test", i)).Scan(&userID); err != nil {
			t.Fatalf("seed user %d: %v", i, err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO notification_preference (workspace_id, user_id, preferences) VALUES ($1, $2, $3::jsonb) RETURNING id`,
			testWorkspaceID, userID, prefs).Scan(&ids[i]); err != nil {
			t.Fatalf("seed preference %d: %v", i, err)
		}
	}

	check := func(step string, want []string) {
		t.Helper()
		for i, id := range ids {
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT preferences FROM notification_preference WHERE id = $1`, id).Scan(&raw); err != nil {
				t.Fatalf("%s row %d: %v", step, i, err)
			}
			var got, expected map[string]string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(want[i]), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("%s row %d: preferences = %s, want %s", step, i, raw, want[i])
			}
		}
	}

	if _, err := tx.Exec(ctx, string(up)); err != nil {
		t.Fatalf("up: %v", err)
	}
	check("up", afterUp)
	if _, err := tx.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down: %v", err)
	}
	check("down", afterDown)
}
