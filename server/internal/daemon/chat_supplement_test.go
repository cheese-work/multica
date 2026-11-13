package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestTaskSupplementCapabilitiesByTaskKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want []string
	}{
		{"issue task", Task{IssueID: "issue-1"}, []string{protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementPREventV1}},
		{"chat task", Task{ChatSessionID: "session-1"}, []string{protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1}},
		{"neither", Task{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskSupplementCapabilities(tc.task); !slices.Equal(got, tc.want) {
				t.Fatalf("taskSupplementCapabilities = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestClientStartTaskAcceptsChatSupplementNegotiation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"supplement_capability":"task-supplement-chat-v1"}`))
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL).StartTask(context.Background(), Task{ID: "task-1", ChatSessionID: "session-1"},
		protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1)
	if err != nil || got != (TaskNegotiation{Supplement: true}) {
		t.Fatalf("StartTask = %+v, %v; want supplement negotiated", got, err)
	}
}
