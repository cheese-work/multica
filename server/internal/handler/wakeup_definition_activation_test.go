package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// useWakeupActivation gives the test its own activation row, so a completed
// activation never leaks into another test or package sharing the database.
func useWakeupActivation(t *testing.T) *service.IssueWakeupService {
	t.Helper()
	key := "handler_test_" + t.Name()
	previous := testHandler.WakeupActivationKey
	testHandler.WakeupActivationKey = key
	dbfx.Exec(t, `DELETE FROM wakeup_definition_activation WHERE name = $1`, key)
	t.Cleanup(func() {
		testHandler.WakeupActivationKey = previous
		dbfx.Exec(t, `DELETE FROM wakeup_definition_activation WHERE name = $1`, key)
	})
	return &service.IssueWakeupService{Tasks: testHandler.TaskService, ActivationKey: key}
}

func completeActivation(t *testing.T, s *service.IssueWakeupService) {
	t.Helper()
	res, err := s.ActivateWakeupDefinitions(context.Background(), service.ActivationRequest{Page: 50, MaxPages: 10, WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil || !res.Complete {
		t.Fatalf("activation = %+v %v", res, err)
	}
}

// Writes follow the completed activation with no flag and no operator step.
func TestWakeupDefinitionWritesOpenWhenTheActivationCompletes(t *testing.T) {
	s := useWakeupActivation(t)
	k := &wdKit{t: t, projectID: dbfx.Project(t, "wd project")}
	wipeWakeupDefinitions(t)
	t.Cleanup(func() { wipeWakeupDefinitions(t) })
	ws := k.workspace()

	ws.put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusForbidden)
	if ws.list().Capabilities.DefinitionWrites {
		t.Fatal("capabilities report writes open before the activation")
	}

	completeActivation(t, s)
	if !ws.list().Capabilities.DefinitionWrites {
		t.Fatal("capabilities still report writes closed after the activation")
	}
	ws.put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusOK)
}

// An explicit off rule is the kill switch once the activation is done.
func TestWakeupDefinitionWritesKillSwitchBeatsTheActivation(t *testing.T) {
	s := useWakeupActivation(t)
	k := &wdKit{t: t, projectID: dbfx.Project(t, "wd project")}
	wipeWakeupDefinitions(t)
	t.Cleanup(func() { wipeWakeupDefinitions(t) })
	completeActivation(t, s)
	withFeatureFlag(t, testHandler, featureflags.WakeupDefinitionWrites, false)

	k.workspace().put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusForbidden)
	if k.workspace().list().Capabilities.DefinitionWrites {
		t.Fatal("kill switch did not close the capability")
	}
}

// The legacy child-done endpoint keeps working before and after activation: an
// older client never needs the new gate.
func TestLegacyChildDoneEndpointIsIndependentOfTheActivation(t *testing.T) {
	s := useWakeupActivation(t)
	call := func(label string) {
		issue := dbfx.Issue(t, "legacy child-done parent "+label, testutil.Cols{"number": nextWorkspaceIssueNumber(t)})
		t.Cleanup(func() { cleanupChildDoneIssue(issue) })
		if w := putChildDoneRule(t, issue, map[string]any{"enabled": false}); w.Code != http.StatusOK {
			t.Fatalf("%s: legacy child-done update = %d %s", label, w.Code, w.Body.String())
		}
	}
	call("before")
	completeActivation(t, s)
	call("after")
}

// A flag cannot skip the backfill: an explicit on rule leaves the gate closed
// until the activation completed.
func TestWakeupDefinitionWritesEnabledFlagCannotOpenTheGateEarly(t *testing.T) {
	s := useWakeupActivation(t)
	k := &wdKit{t: t, projectID: dbfx.Project(t, "wd project")}
	wipeWakeupDefinitions(t)
	t.Cleanup(func() { wipeWakeupDefinitions(t) })
	withFeatureFlag(t, testHandler, featureflags.WakeupDefinitionWrites, true)

	k.workspace().put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusForbidden)
	if k.workspace().list().Capabilities.DefinitionWrites {
		t.Fatal("an enabled flag reported writes open before the activation completed")
	}
	completeActivation(t, s)
	k.workspace().put("child_done", 0, cfg(map[string]any{"name": "x"})).Want(http.StatusOK)
}
