package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// setCardLint stores decision_card_lint_mode for the test workspace.
func setCardLint(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings, '{}') || jsonb_build_object('decision_card_lint_mode', $1::text) WHERE id = $2`, mode, testWorkspaceID); err != nil {
		t.Fatalf("set decision_card_lint_mode: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'decision_card_lint_mode' WHERE id = $1`, testWorkspaceID)
	})
}

// weakConAsk is a card whose first option has only a time con.
func weakConAsk(fixture requestCoreFixture) map[string]any {
	ask := fixture.newShapeAsk()
	ask["options"].([]any)[0].(map[string]any)["cons"] = []string{"Takes 1 minute of your time"}
	return ask
}

func TestDecisionRequestCardLintBlockWarnOff(t *testing.T) {
	setCannotDo(t, "off", "")
	t.Run("block refuses with code and field path", func(t *testing.T) {
		setCardLint(t, "block")
		fixture := newRequestCoreFixture(t, false)
		res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, weakConAsk(fixture))).Want(http.StatusBadRequest)
		if !strings.Contains(res.Text(), "ask.options[0].cons weak_cons:") {
			t.Errorf("400 body lacks code and field path: %s", res.Text())
		}
		fixture.assertCounts(t, 0)
	})
	t.Run("warn creates the card with a hint", func(t *testing.T) {
		setCardLint(t, "warn")
		fixture := newRequestCoreFixture(t, false)
		var created struct {
			Hints []string `json:"hints"`
		}
		testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, weakConAsk(fixture))).Want(http.StatusCreated).JSON(&created)
		if len(created.Hints) == 0 || !strings.Contains(strings.Join(created.Hints, "\n"), "weak_cons") {
			t.Errorf("hints = %q, want weak_cons", created.Hints)
		}
	})
	t.Run("off leaves today's behaviour", func(t *testing.T) {
		fixture := newRequestCoreFixture(t, false)
		var created struct {
			Hints []string `json:"hints"`
		}
		testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, weakConAsk(fixture))).Want(http.StatusCreated).JSON(&created)
		if strings.Contains(strings.Join(created.Hints, "\n"), "weak_cons") {
			t.Errorf("off must not lint: %q", created.Hints)
		}
	})
}

func TestValidateCardLintSetting(t *testing.T) {
	for value, ok := range map[string]any{"off": true, "warn": true, "block": true, "on": false, "": false} {
		if err := validateCardLintSetting(map[string]any{"decision_card_lint_mode": value}); (err == nil) != ok.(bool) {
			t.Errorf("%q: err = %v, want ok=%v", value, err, ok)
		}
	}
	if err := validateCardLintSetting(map[string]any{"decision_card_lint_mode": nil}); err == nil {
		t.Error("null must be refused")
	}
	if err := validateCardLintSetting(map[string]any{}); err != nil {
		t.Errorf("absent key must be valid: %v", err)
	}
}
