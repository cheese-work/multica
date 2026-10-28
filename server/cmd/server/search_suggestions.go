package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/handler"
)

func registerSearchSuggestionsRoutes(router chi.Router, handlers *handler.Handler, authenticate, membership func(http.Handler) http.Handler) {
	router.Group(func(group chi.Router) {
		group.Use(handlers.SearchSuggestions.Observe)
		group.Use(handlers.SearchSuggestions.Admit)
		group.Use(authenticate)
		group.Use(membership)
		group.Use(handler.RequireHumanActor)
		group.Use(handlers.SearchSuggestions.Limit)
		group.Get("/api/search/suggestions", handlers.SuggestSearch)
	})
}
