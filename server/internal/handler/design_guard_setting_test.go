package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

const designGuardOK = `{"mode":"warn","doop_origin":"https://doop.example.com","design_url_property_id":"b73a1103-30d7-4f9b-9412-4edcd64f5232"}`

func TestValidateDesignGuardSetting(t *testing.T) {
	for name, v := range map[string]any{
		"not a map":  "x",
		"absent key": map[string]any{},
		"off":        map[string]any{"design_card_guard": map[string]any{"mode": "off"}},
		"warn":       map[string]any{"design_card_guard": map[string]any{"mode": "warn", "doop_origin": "https://doop.example.com", "design_url_property_id": "b73a1103-30d7-4f9b-9412-4edcd64f5232"}},
	} {
		if err := validateDesignGuardSetting(v); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	for name, v := range map[string]any{
		"null":            map[string]any{"design_card_guard": nil},
		"string":          map[string]any{"design_card_guard": "block"},
		"bad mode":        map[string]any{"design_card_guard": map[string]any{"mode": "on"}},
		"block no origin": map[string]any{"design_card_guard": map[string]any{"mode": "block"}},
		"unknown field":   map[string]any{"design_card_guard": map[string]any{"mode": "off", "x": 1}},
	} {
		if err := validateDesignGuardSetting(v); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestUpdateWorkspaceValidatesDesignGuardSetting(t *testing.T) {
	wsID := pauseWorkspaceFixture(t, "design-guard-save")
	save := func(guard any) *testutil.Response {
		req := withURLParam(newRequest("PATCH", "/api/workspaces/"+wsID, map[string]any{"settings": map[string]any{"design_card_guard": guard}}), "id", wsID)
		return testutil.Call(t, testHandler.UpdateWorkspace, req)
	}
	res := save(map[string]any{"mode": "block"}).Want(http.StatusBadRequest)
	if !strings.Contains(res.Text(), "design_card_guard") {
		t.Errorf("400 body does not name the key: %s", res.Text())
	}
	save("block").Want(http.StatusBadRequest)
	save(nil).Want(http.StatusBadRequest)
	save(map[string]any{"mode": "off"}).Want(http.StatusOK)
}

func TestSettingsPatchValueDesignGuard(t *testing.T) {
	if err := validateSettingsPatchValue("design_card_guard", []byte(designGuardOK)); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
	if err := validateSettingsPatchValue("design_card_guard", []byte(`{"mode":"on"}`)); err == nil {
		t.Error("bad mode accepted")
	}
}
