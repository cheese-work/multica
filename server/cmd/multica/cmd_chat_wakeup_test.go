package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func chatWakeupTestCommand(after, at, instruction string) *cobra.Command {
	command := &cobra.Command{Use: "create"}
	command.Flags().String("after", after, "")
	command.Flags().String("at", at, "")
	command.Flags().String("instruction", instruction, "")
	command.Flags().String("title", "", "")
	command.Flags().String("output", "json", "")
	return command
}

func TestChatWakeupCreateSendsOptionalTitle(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"id":"w1","title":"Check deploy"}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)

	if chatWakeupCreateCmd.Flags().Lookup("title") == nil || !strings.Contains(chatWakeupCmd.Long, "--title") {
		t.Error("create command must register --title and the wakeup help must mention it")
	}
	titled := chatWakeupTestCommand("25m", "", "refresh")
	_ = titled.Flags().Set("title", "Check deploy")
	titled.SetOut(&bytes.Buffer{})
	if err := runChatWakeupCreate(titled, nil); err != nil {
		t.Fatal(err)
	}
	if body["title"] != "Check deploy" {
		t.Fatalf("titled body = %v, want title", body)
	}

	body = nil
	untitled := chatWakeupTestCommand("25m", "", "refresh")
	untitled.SetOut(&bytes.Buffer{})
	if err := runChatWakeupCreate(untitled, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := body["title"]; has {
		t.Fatalf("untitled body = %v, want no title key", body)
	}
}

func TestChatWakeupCreatePostsDelayAndInstruction(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/chat/wakeups" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil ||
			body["after_seconds"] != float64(1500) || body["instruction"] != "refresh Luna readiness" {
			t.Errorf("request body = %v, %v", body, err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"id":"w1","fire_at":"2026-10-26T04:40:00Z","instruction":"refresh Luna readiness","status":"pending"}`))
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	command := chatWakeupTestCommand("25m", "", "refresh Luna readiness")
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runChatWakeupCreate(command, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id": "w1"`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestChatWakeupCreateRejectsBadTimingWithoutHTTP(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("no HTTP request expected")
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	for name, command := range map[string]*cobra.Command{
		"neither":      chatWakeupTestCommand("", "", "x"),
		"both":         chatWakeupTestCommand("5m", "2026-10-26T04:40:00Z", "x"),
		"bad duration": chatWakeupTestCommand("soon", "", "x"),
		"bad time":     chatWakeupTestCommand("", "tomorrow", "x"),
		"no text":      chatWakeupTestCommand("5m", "", " "),
	} {
		if err := runChatWakeupCreate(command, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestChatWakeupCancelDeletesByID(t *testing.T) {
	clearAmbientDaemonTaskEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/api/chat/wakeups/w1" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	setCLITestServerEnv(t, server.URL)
	command := &cobra.Command{Use: "cancel"}
	if err := runChatWakeupCancel(command, []string{"w1"}); err != nil {
		t.Fatal(err)
	}
}
