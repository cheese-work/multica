package main

import (
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/realtime"
)

// CHE-3702 round 20: a task token reaches chat content through any /api/chat*
// route, so every one of them carries a class in handler.ChatRouteClasses (the
// ChatTaskTokenGate map). A new route fails here until someone classes it, and
// the gate answers 403 for an unclassed route in the meantime.
func TestEveryChatRouteHasATaskTokenClass(t *testing.T) {
	router := NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil)
	var routes []string
	var ungated []string
	_ = chi.Walk(router, func(method, route string, _ http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		if route != "/api/chat" && !strings.HasPrefix(route, "/api/chat/") {
			return nil
		}
		routes = append(routes, method+" "+route)
		gated := false
		for _, mw := range middlewares {
			gated = gated || strings.HasSuffix(runtime.FuncForPC(reflect.ValueOf(mw).Pointer()).Name(), "ChatTaskTokenGate-fm")
		}
		if !gated {
			ungated = append(ungated, method+" "+route)
		}
		// The gate resolves the pattern with Match; it must equal the walked route.
		probe := chi.NewRouteContext()
		path := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(route, "x")
		if !router.(chi.Routes).Match(probe, method, path) || strings.TrimSuffix(probe.RoutePattern(), "/") != strings.TrimSuffix(route, "/") {
			t.Errorf("%s %s resolves to %q, the gate would not find its class", method, route, probe.RoutePattern())
		}
		return nil
	})
	if len(ungated) > 0 {
		t.Errorf("chat routes outside ChatTaskTokenGate:\n%q", ungated)
	}
	classes := handler.ChatRouteClasses()
	var unclassed, stale []string
	for _, route := range routes {
		if _, ok := classes[route]; !ok {
			unclassed = append(unclassed, route)
		}
	}
	for route := range classes {
		if !slices.Contains(routes, route) {
			stale = append(stale, route)
		}
	}
	sort.Strings(unclassed)
	sort.Strings(stale)
	if len(unclassed) > 0 {
		t.Errorf("chat routes without a task-token class; add them to chatRouteClasses in chat_task_token_gate.go:\n%q", unclassed)
	}
	if len(stale) > 0 {
		t.Errorf("classed chat routes the router no longer has; remove them from chatRouteClasses:\n%q", stale)
	}
	if len(routes) == 0 {
		t.Fatal("no /api/chat routes found")
	}
}
