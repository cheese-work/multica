package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// setHeldSettingMode stores the CHE-5951 mode and one held setting.
func setHeldSettingMode(t *testing.T, mode string) {
	t.Helper()
	ctx := context.Background()
	patch := `{"held_setting_ask_mode":"` + mode + `","compact_prompt":"secret-value-do-not-echo"}`
	if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = COALESCE(settings, '{}') || $1::jsonb WHERE id = $2`, patch, testWorkspaceID); err != nil {
		t.Fatalf("set held_setting_ask_mode: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = settings - 'held_setting_ask_mode' - 'compact_prompt' WHERE id = $1`, testWorkspaceID)
	})
}

func TestDecisionRequestBlockRefusesAskForAHeldSetting(t *testing.T) {
	setHeldSettingMode(t, "block")
	fixture := newRequestCoreFixture(t, false)
	ask := fixture.newShapeAsk()
	ask["settings_needed"] = []any{"compact_prompt"}
	res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask)).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), `ask.settings_needed[0] \"compact_prompt\" is already set in workspace settings`) {
		t.Errorf("body = %s", res.Text())
	}
	if strings.Contains(res.Text(), "secret-value-do-not-echo") {
		t.Errorf("body echoes the stored value: %s", res.Text())
	}
	fixture.assertCounts(t, 0)
}

func TestDecisionRequestBlockAcceptsAnAskForAnUnsetSetting(t *testing.T) {
	setHeldSettingMode(t, "block")
	fixture := newRequestCoreFixture(t, false)
	ask := fixture.newShapeAsk()
	ask["settings_needed"] = []any{"never_set_key"}
	testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask)).Want(http.StatusCreated)
}

func TestDecisionRequestWarnCreatesCardWithAHeldSettingHint(t *testing.T) {
	for mode, wantHint := range map[string]bool{"warn": true, "off": false} {
		t.Run(mode, func(t *testing.T) {
			setHeldSettingMode(t, mode)
			fixture := newRequestCoreFixture(t, false)
			ask := fixture.newShapeAsk()
			ask["settings_needed"] = []any{"compact_prompt"}
			res := testutil.Call(t, testHandler.RequestDecision, fixture.askRequest(t, ask)).Want(http.StatusCreated)
			if got := strings.Contains(res.Text(), "already set in workspace settings"); got != wantHint {
				t.Errorf("hint present = %v, want %v: %s", got, wantHint, res.Text())
			}
		})
	}
}

func TestValidateHeldSettingModeSetting(t *testing.T) {
	for value, wantErr := range map[string]bool{"off": false, "warn": false, "block": false, "loud": true, "": true} {
		err := validateHeldSettingModeSetting(map[string]any{"held_setting_ask_mode": value})
		if (err != nil) != wantErr {
			t.Errorf("%q: err = %v, wantErr %v", value, err, wantErr)
		}
	}
	if err := validateHeldSettingModeSetting(map[string]any{"held_setting_ask_mode": nil}); err == nil {
		t.Error("null must be refused")
	}
	if err := validateHeldSettingModeSetting(map[string]any{"other": 1}); err != nil {
		t.Errorf("absent key: %v", err)
	}
}
