package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func pauseDecisionTransaction(test *testing.T, query string) (<-chan int, func()) {
	test.Helper()
	paused, release := make(chan int, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	test.Cleanup(unblock)
	starter := &requestCoreStarter{base: testHandler.TxStarter, query: query, before: func(transaction pgx.Tx) error {
		var backendPID int
		if err := transaction.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		paused <- backendPID
		select {
		case <-release:
			return nil
		case <-time.After(8 * time.Second):
			return errors.New("decision transaction probe timed out")
		}
	}}
	useRequestCoreStarter(test, starter)
	return paused, unblock
}

func awaitDecisionPause(test *testing.T, paused <-chan int) int {
	test.Helper()
	select {
	case backendPID := <-paused:
		return backendPID
	case <-time.After(4 * time.Second):
		test.Fatal("decision transaction did not reach its fence")
		return 0
	}
}

func decisionSessionRequestForHuman(test *testing.T, fixture requestCoreFixture, action string) *http.Request {
	test.Helper()
	request := decisionGuardRequest(test, fixture.sessionID, action)
	request.Header.Set("X-User-ID", fixture.humanID)
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(request.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: parseUUID(fixture.humanID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	return request.WithContext(middleware.SetMemberContext(request.Context(), testWorkspaceID, member))
}

func startDecisionSessionAction(test *testing.T, handler *Handler, request *http.Request, action string) <-chan int {
	test.Helper()
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	test.Cleanup(cancel)
	finished := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		if action == "archive" {
			handler.SetChatSessionArchived(response, request.WithContext(ctx))
		} else if action == "continue" {
			handler.ContinueChatSession(response, request.WithContext(ctx))
		} else {
			handler.DeleteChatSession(response, request.WithContext(ctx))
		}
		finished <- response.Code
	}()
	return finished
}

func awaitDecisionSessionAction(test *testing.T, finished <-chan int) int {
	test.Helper()
	select {
	case status := <-finished:
		return status
	case <-time.After(10 * time.Second):
		test.Fatal("session action did not finish")
		return 0
	}
}

func TestDecisionPingArchiveOrders(test *testing.T) {
	for _, action := range []string{"archive", "continue"} {
		for _, first := range []string{"ping", "archive"} {
			test.Run(action+"/"+first+"_first", func(test *testing.T) {
				fixture := newRequestCoreFixture(test, false)
				original := *testHandler
				request := decisionSessionRequestForHuman(test, fixture, action)
				query := "-- name: CreateDecisionRequest"
				if first == "archive" {
					query = "-- name: DecisionOpenLaneExists"
				}
				paused, unblock := pauseDecisionTransaction(test, query)
				var ping <-chan requestCoreOutcome
				var archive <-chan int
				if first == "ping" {
					ping = startRequestCore(test, fixture, fixture.input())
				} else {
					archive = startDecisionSessionAction(test, testHandler, request, action)
				}
				holderPID := awaitDecisionPause(test, paused)
				if first == "ping" {
					archive = startDecisionSessionAction(test, &original, request, action)
				} else {
					finished := make(chan requestCoreOutcome, 1)
					go func() {
						result, err := original.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
						finished <- requestCoreOutcome{result, err}
					}()
					ping = finished
				}
				if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
					test.Fatal("second operation bypassed the session fence")
				}
				unblock()
				outcome, status := awaitRequestCore(test, ping), awaitDecisionSessionAction(test, archive)
				if first == "ping" {
					if outcome.err != nil || status != http.StatusConflict {
						test.Fatalf("ping-first outcome: %v, archive %d", outcome.err, status)
					}
					fixture.assertCounts(test, 1)
				} else {
					wanted := http.StatusOK
					if action == "continue" {
						wanted = http.StatusCreated
					}
					if outcome.err == nil || status != wanted {
						test.Fatalf("archive-first outcome: %v, archive %d", outcome.err, status)
					}
					fixture.assertCounts(test, 0)
				}
			})
		}
	}
}
