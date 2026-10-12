package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestReportTaskUsageKeepsLastRequestSeparate(t *testing.T) {
	want := agent.ModelRequestUsage{Provider: "claude", Model: "claude-sonnet-4-6", InputTokens: 2000, CacheReadTokens: 160000, ContextWindow: 200000, CLICompactionTokens: 190000}
	called := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Usage            []TaskUsageEntry        `json:"usage"`
			LastModelRequest agent.ModelRequestUsage `json:"last_model_request"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.LastModelRequest != want || len(body.Usage) != 1 || body.Usage[0].InputTokens != 900000 {
			t.Errorf("request: %+v", body)
		}
		called <- true
		writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := NewClient(server.URL)
	if err := client.ReportTaskUsage(context.Background(), "test-task", []TaskUsageEntry{{Provider: "claude", Model: "claude-sonnet-4-6", InputTokens: 900000}}, &want); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	default:
		t.Fatal("last request was not sent")
	}
}
