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

	// 1. Separate ServerVersion, ServerBuild, and ServerCommit (C00 CD / release build).
	handler := &Handler{
		cfg: Config{
			ServerVersion: "0.6.3",
			ServerBuild:   "20261021-0856",
			ServerCommit:  "1de8883940587ab1234567890abcdef123456789",
		},
		FeatureFlags: featureflag.NewService(featureflag.NewStaticProvider()),
	}
	response := httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var config map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	if response.Code != http.StatusOK ||
		config["server_version"] != "0.6.3" ||
		config["server_build"] != "20261021-0856" ||
		config["server_commit"] != "1de8883" {
		test.Fatalf("config response = %d %#v, want server_version=0.6.3 server_build=20261021-0856 server_commit=1de8883", response.Code, config)
	}
	for _, legacy := range []string{"version", "build"} {
		if _, exists := config[legacy]; exists {
			test.Errorf("unexpected legacy field %q in /api/config: %#v", legacy, config)
		}
	}

	// 2. Upstream release without a fork build stamp omits server_build while keeping server_version and server_commit.
	handler.cfg = Config{
		ServerVersion: "0.6.3",
		ServerCommit:  "1de8883",
	}
	response = httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	config = nil
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	if config["server_version"] != "0.6.3" || config["server_commit"] != "1de8883" {
		test.Fatalf("upstream release config = %#v, want server_version=0.6.3 server_commit=1de8883", config)
	}
	if _, exists := config["server_build"]; exists {
		test.Errorf("upstream release without fork stamp must omit server_build, got %#v", config["server_build"])
	}

	// 3. Combined <upstream>-<stamp> in ServerVersion splits cleanly into server_version and server_build.
	handler.cfg = Config{
		ServerVersion: "0.6.3-20261021-0856",
		ServerCommit:  "1de8883",
	}
	response = httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	config = nil
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	if config["server_version"] != "0.6.3" || config["server_build"] != "20261021-0856" || config["server_commit"] != "1de8883" {
		test.Fatalf("split config = %#v, want server_version=0.6.3 server_build=20261021-0856 server_commit=1de8883", config)
	}

	// 4. Dev build (empty ServerVersion) omits all three fields even if ServerBuild or ServerCommit is set.
	handler.cfg = Config{
		ServerVersion: "",
		ServerBuild:   "20261021-0856",
		ServerCommit:  "1de8883",
	}
	response = httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	config = nil
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{"server_version", "server_build", "server_commit"} {
		if _, exists := config[field]; exists {
			test.Errorf("dev build exposed %s: %#v", field, config[field])
		}
	}

	// 5. Official cloud (multica.ai) omits all three fields.
	handler.cfg = Config{
		ServerVersion: "0.6.3",
		ServerBuild:   "20261021-0856",
		ServerCommit:  "1de8883",
	}
	test.Setenv("MULTICA_APP_URL", "https://multica.ai")
	response = httptest.NewRecorder()
	handler.GetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	config = nil
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{"server_version", "server_build", "server_commit"} {
		if _, exists := config[field]; exists {
			test.Errorf("official cloud exposed %s", field)
		}
	}
}
