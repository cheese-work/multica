package handler

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
)

// chatRouteClass says what a task token may do on a /api/chat* route.
type chatRouteClass int

const (
	// chatRouteContent returns chat message text. The reading task takes the
	// chat's taint after the handler read the transcript and before the body
	// leaves the server.
	chatRouteContent chatRouteClass = iota + 1
	// chatRouteMetadata returns no message text (ids, titles, counts, state).
	chatRouteMetadata
	// chatRouteWrite changes chat state and returns no transcript.
	chatRouteWrite
	// chatRouteDeny is closed to task tokens: it returns transcript text and no
	// agent-side caller needs it.
	chatRouteDeny
)

// chatRouteClasses is keyed "METHOD /pattern" exactly as chi.Walk reports the
// route. A /api/chat* route missing here is closed to task tokens, and
// TestEveryChatRouteHasATaskTokenClass fails until it is added.
var chatRouteClasses = map[string]chatRouteClass{
	"GET /api/chat/history": chatRouteContent,
	"GET /api/chat/thread":  chatRouteContent,

	"GET /api/chat/sessions/{sessionId}/messages":       chatRouteDeny,
	"GET /api/chat/sessions/{sessionId}/messages/page":  chatRouteDeny,
	"GET /api/chat/sessions/{sessionId}/segments":       chatRouteContent,
	"GET /api/chat/sessions/{sessionId}/segment":        chatRouteContent,
	"GET /api/chat/sessions/{sessionId}/pending-task":   chatRouteDeny,
	"GET /api/chat/sessions/{sessionId}/draft-restores": chatRouteDeny,
	"GET /api/chat/search":                              chatRouteDeny,

	// DM attention (CHE-4147): human-only; the routes also sit behind humanOnly.
	"GET /api/chat/sessions/{sessionId}/attention":        chatRouteDeny,
	"POST /api/chat/sessions/{sessionId}/attention/xong":  chatRouteDeny,
	"POST /api/chat/sessions/{sessionId}/attention/later": chatRouteDeny,
	"POST /api/chat/attention/xong-all":                   chatRouteDeny,
	"POST /api/chat/attention/xong-all/{batchId}/undo":    chatRouteDeny,

	"GET /api/chat/sessions/":                    chatRouteMetadata,
	"GET /api/chat/sessions/{sessionId}/":        chatRouteMetadata,
	"GET /api/chat/sessions/{sessionId}/usage":   chatRouteMetadata,
	"GET /api/chat/sessions/{sessionId}/wakeups": chatRouteMetadata,
	"GET /api/chat/pending-tasks":                chatRouteMetadata,
	"GET /api/chat/pending-tasks/has-any":        chatRouteMetadata,
	"GET /api/chat/pinned-agents":                chatRouteMetadata,
	"GET /api/chat/wakeups":                      chatRouteMetadata,

	"POST /api/chat/sessions/":                                                   chatRouteWrite,
	"PATCH /api/chat/sessions/{sessionId}/":                                      chatRouteWrite,
	"DELETE /api/chat/sessions/{sessionId}/":                                     chatRouteWrite,
	"PATCH /api/chat/sessions/{sessionId}/pin":                                   chatRouteWrite,
	"PATCH /api/chat/sessions/{sessionId}/archive":                               chatRouteWrite,
	"DELETE /api/chat/sessions/{sessionId}/wakeups/{wakeupId}":                   chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/messages":                               chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/continue":                               chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/fork":                                   chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/onboarding":                             chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/quick-actions/regenerate":               chatRouteWrite,
	"DELETE /api/chat/sessions/{sessionId}/queued-tasks":                         chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/queued-tasks/{taskId}/prioritize":       chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/tasks/{taskId}/steer":                   chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/messages/{messageId}/supplement/resend": chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/read":                                   chatRouteWrite,
	"POST /api/chat/sessions/{sessionId}/unread":                                 chatRouteWrite,
	"DELETE /api/chat/sessions/{sessionId}/draft-restores/{restoreId}":           chatRouteWrite,
	"POST /api/chat/pinned-agents":                                               chatRouteWrite,
	"DELETE /api/chat/pinned-agents/{agentId}":                                   chatRouteWrite,
	"POST /api/chat/continue":                                                    chatRouteWrite,
	"POST /api/chat/compact":                                                     chatRouteWrite,
	"POST /api/chat/outcome":                                                     chatRouteWrite,
	"POST /api/chat/wakeups":                                                     chatRouteWrite,
	"DELETE /api/chat/wakeups/{wakeupId}":                                        chatRouteWrite,
	"POST /api/chat/fork":                                                        chatRouteWrite,
}

// ChatRouteClasses exposes the class map to the router audit test.
func ChatRouteClasses() map[string]chatRouteClass { return chatRouteClasses }

// ChatTaskTokenGate is router middleware for every /api/chat* route. It acts
// only on task-token callers (X-Actor-Source is server-stamped). The group
// middleware runs before the sub-router matches, so it resolves the leaf
// pattern itself and looks it up in chatRouteClasses; an unclassed route is 403.
//
// A content route is buffered: the handler reads the transcript, then the
// reading task takes the chat's taint, then the body is sent. Taint only grows
// and a turn commits its message and task together, so the taint read after the
// transcript covers every message in the body. Recording first would let a turn
// commit in between (CHE-3702). If recording fails the body is dropped (500).
func (h *Handler) ChatTaskTokenGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Actor-Source") != "task_token" || !isChatPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		class, ok := chatRouteClassOf(r)
		if !ok || class == chatRouteDeny {
			writeError(w, http.StatusForbidden, "this chat route is not available to an agent task")
			return
		}
		if class != chatRouteContent {
			next.ServeHTTP(w, r)
			return
		}
		buf := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(buf, r)
		if buf.status >= 200 && buf.status < 300 {
			if !h.recordReadingTaskTaint(w, r) {
				return
			}
		}
		buf.flushTo(w)
	})
}

func isChatPath(path string) bool {
	return path == "/api/chat" || strings.HasPrefix(path, "/api/chat/")
}

func chatRouteClassOf(r *http.Request) (chatRouteClass, bool) {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil || rctx.Routes == nil {
		return 0, false
	}
	probe := chi.NewRouteContext()
	if !rctx.Routes.Match(probe, r.Method, r.URL.Path) {
		return 0, false
	}
	// A mounted Route root reports its pattern without the trailing slash that
	// chi.Walk (and the map) carry.
	pattern := r.Method + " " + probe.RoutePattern()
	if class, ok := chatRouteClasses[pattern]; ok {
		return class, true
	}
	class, ok := chatRouteClasses[pattern+"/"]
	return class, ok
}

// recordReadingTaskTaint makes the calling task take its chat's taint.
func (h *Handler) recordReadingTaskTaint(w http.ResponseWriter, r *http.Request) bool {
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err == nil {
		task, getErr := h.Queries.GetAgentTask(r.Context(), taskID)
		if getErr == nil {
			err = h.recordChatTaint(r.Context(), &task, "")
		} else {
			err = getErr
		}
	}
	if err != nil {
		slog.Error("chat content read: record chat taint failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to record chat taint")
		return false
	}
	return true
}

// bufferedResponse holds a handler's response until the gate decides to send it.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }

func (b *bufferedResponse) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
