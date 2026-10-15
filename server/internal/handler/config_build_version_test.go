package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/featureflag"
)

func TestGetConfigSeparatesReleaseBuildVersion(test *testing.T) {
	test.Setenv("MULTICA_APP_URL", "https://multica.self-hosted.example")
	test.Setenv("FRONTEND_ORIGIN", "")
	handler := &Handler{
		cfg:          Config{ServerVersion: "0.6.0-20261008-1331"},
		FeatureFlags: featureflag.NewService(featureflag.NewStaticProvider()),
	}
	response := httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var config map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	if response.Code != http.StatusOK || config["version"] != "0.6.0" || config["build"] != "20261008-1331" || config["server_version"] != "0.6.0-20261008-1331" {
		test.Fatalf("config response = %d %#v, want separate plain version/build and unchanged display", response.Code, config)
	}
	test.Setenv("MULTICA_APP_URL", "https://multica.ai")
	response = httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	config = nil
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{"version", "build", "server_version"} {
		if _, exists := config[field]; exists {
			test.Errorf("official cloud exposed %s", field)
		}
	}
}
