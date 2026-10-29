package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/redis/go-redis/v9"
)

const suggestionsTimeout = 1500 * time.Millisecond

type suggestionsResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (response *suggestionsResponse) Header() http.Header { return response.header }
func (response *suggestionsResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}
func (response *suggestionsResponse) Write(data []byte) (int, error) {
	response.WriteHeader(http.StatusOK)
	return response.body.Write(data)
}

type SearchSuggestions struct {
	enabled bool
	slots   chan struct{}
	limiter SlidingWindowRateLimiter
	metrics *obsmetrics.HTTPMetrics
}

func NewSearchSuggestions(enabled bool, maxConcurrent, rate int, redis redis.UniversalClient, metrics *obsmetrics.HTTPMetrics) *SearchSuggestions {
	config := SlidingWindowRateLimit{Limit: rate, Window: time.Minute}
	limiter := newMemorySlidingWindowRateLimiter(config)
	if redis != nil {
		limiter = newRedisSlidingWindowRateLimiter(redis, config, "mul:ratelimit:search-suggestions:")
	}
	return &SearchSuggestions{enabled: enabled, slots: make(chan struct{}, maxConcurrent), limiter: limiter, metrics: metrics}
}

func writeSuggestions(writer http.ResponseWriter, query string, rows []string, outcome string) {
	if rows == nil {
		rows = []string{}
	}
	writer.Header().Set("Content-Type", "application/x-suggestions+json")
	writer.Header().Set("X-Search-Suggestions-Outcome", outcome)
	_ = json.NewEncoder(writer).Encode([]any{query, rows, []string{}, []string{}})
}

func (config *SearchSuggestions) Observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		ctx, cancel := context.WithTimeout(request.Context(), suggestionsTimeout)
		defer cancel()
		response := &suggestionsResponse{header: make(http.Header)}
		next.ServeHTTP(response, request.WithContext(ctx))
		response.WriteHeader(http.StatusOK)
		outcome := response.Header().Get("X-Search-Suggestions-Outcome")
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			outcome = "cancelled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			outcome = "timeout"
		case outcome != "":
		case response.status == 400:
			outcome = "bad_request"
		case response.status == 401:
			outcome = "unauthenticated"
		case response.status == 403:
			outcome = "forbidden"
		case response.status == 404:
			outcome = "not_member"
		default:
			outcome = "error"
		}
		if outcome == "timeout" || outcome == "cancelled" || outcome == "error" {
			response.body.Reset()
			response.status = 200
			writeSuggestions(response, request.URL.Query().Get("q"), nil, outcome)
		}
		for name, values := range response.Header() {
			writer.Header()[name] = values
		}
		writer.Header().Del("Access-Control-Allow-Origin")
		writer.Header().Del("Access-Control-Allow-Credentials")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Vary", "Cookie")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Search-Suggestions-Outcome", outcome)
		writer.WriteHeader(response.status)
		_, _ = writer.Write(response.body.Bytes())
		duration := time.Since(started)
		slog.Info("search suggestions", "outcome", outcome, "duration", duration)
		config.metrics.RecordSearchSuggestions(outcome, duration)
	})
}

func (config *SearchSuggestions) Admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query().Get("q")
		length := utf8.RuneCountInString(strings.TrimSpace(query))
		outcome := ""
		switch {
		case !config.enabled:
			outcome = "disabled"
		case length < 2:
			outcome = "too_short"
		case length > 200:
			outcome = "too_long"
		case request.URL.Query().Get("workspace_slug") == "":
			writeError(writer, http.StatusBadRequest, "workspace_slug is required")
			return
		}
		if outcome != "" {
			writeSuggestions(writer, query, nil, outcome)
			return
		}
		request.Header.Del("X-Workspace-Slug")
		request.Header.Del("X-Workspace-ID")
		select {
		case config.slots <- struct{}{}:
			defer func() { <-config.slots }()
		default:
			writeSuggestions(writer, query, nil, "over_cap")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (config *SearchSuggestions) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		key := request.Header.Get("X-User-ID")
		allowed, err := slidingWindowLimiterAllow(request.Context(), config.limiter, key)
		if err == nil && !allowed {
			retry := slidingWindowLimiterRetryAfter(request.Context(), config.limiter, key)
			writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds())+1)))
			writeSuggestions(writer, request.URL.Query().Get("q"), nil, "throttled")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

var suggestionIdentifier = regexp.MustCompile(`(?i)^([a-z0-9]+)-(\d+)$`)

func suggestionNumber(query, prefix string) (int, bool) {
	if match := suggestionIdentifier.FindStringSubmatch(query); match != nil {
		if !strings.EqualFold(match[1], prefix) {
			return 0, false
		}
		query = match[2]
	}
	number, err := strconv.Atoi(query)
	return number, err == nil && number > 0
}

func suggestionText(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	characters := []rune(value)
	if len(characters) > 120 {
		characters = characters[:120]
	}
	return string(characters)
}

// buildSuggestionIssueQuery matches issue titles and identifiers only, so it
// never reads description or comment text (CHE-2402: the full issue search
// scanned ~50k comments and timed out). The LOWER(title) LIKE predicate is
// served by idx_issue_title_trgm; $1 is the workspace, filled by the caller.
func buildSuggestionIssueQuery(phrase string, terms []string, queryNum int, hasNum bool) (string, []any) {
	phrase = strings.ToLower(phrase)
	args := []any{nil, "%" + escapeLike(phrase) + "%", escapeLike(phrase) + "%"}
	match := "LOWER(title) LIKE $2"
	if len(terms) > 1 {
		all := make([]string, len(terms))
		for index, term := range terms {
			args = append(args, "%"+escapeLike(strings.ToLower(term))+"%")
			all[index] = fmt.Sprintf("LOWER(title) LIKE $%d", len(args))
		}
		match += " OR (" + strings.Join(all, " AND ") + ")"
	}
	order := "(LOWER(title) LIKE $3) DESC"
	if hasNum {
		args = append(args, queryNum)
		match += fmt.Sprintf(" OR number = $%d", len(args))
		order = fmt.Sprintf("(number = $%d) DESC, ", len(args)) + order
	}
	return "SELECT number, title FROM issue WHERE workspace_id = $1 AND (" + match + ") ORDER BY " + order + ", updated_at DESC LIMIT 5", args
}

func (handlers *Handler) SuggestSearch(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	raw := request.URL.Query().Get("q")
	query := strings.TrimSpace(raw)
	workspace, ok := parseUUIDOrBadRequest(writer, handlers.resolveWorkspaceID(request), "workspace_id")
	if !ok {
		return
	}
	prefix := handlers.getIssuePrefix(ctx, workspace)
	number, hasNumber := suggestionNumber(query, prefix)
	sql, args := buildSuggestionIssueQuery(query, splitSearchTerms(query), number, hasNumber)
	args[0] = workspace
	var suggestions []string
	err := runSearchQueryWithTimeout(ctx, handlers.TxStarter, sql, args, func(rows pgx.Rows) error {
		for rows.Next() {
			var number int32
			var title string
			if err := rows.Scan(&number, &title); err != nil {
				return err
			}
			suggestions = append(suggestions, suggestionText(fmt.Sprintf("%s-%d %s", prefix, number, title)))
		}
		return rows.Err()
	}, suggestionsTimeout)
	if err == nil {
		sql, args = buildProjectSearchQuery(query, splitSearchTerms(query), true)
		args[1], args[len(args)-2], args[len(args)-1] = workspace, 50, 0
		projects := 0
		err = runSearchQueryWithTimeout(ctx, handlers.TxStarter, sql, args, func(rows pgx.Rows) error {
			for rows.Next() {
				result, err := scanSearchProject(rows)
				if err != nil {
					return err
				}
				if projects < 3 && result.matchSource == "title" {
					suggestions = append(suggestions, suggestionText(result.project.Title))
					projects++
				}
			}
			return rows.Err()
		}, suggestionsTimeout)
	}
	if err != nil {
		outcome := "error"
		if isSearchStatementTimeout(err) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = "timeout"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome = "cancelled"
		}
		writeSuggestions(writer, raw, nil, outcome)
		return
	}
	outcome := "empty"
	if len(suggestions) > 0 {
		outcome = "rows"
	}
	writeSuggestions(writer, raw, suggestions, outcome)
}
