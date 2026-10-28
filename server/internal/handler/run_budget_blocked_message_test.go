package handler

import (
	"strings"
	"testing"
)

func TestRunBudgetBlockedMessageOffersARaiseOnlyWhenOneIsValid(t *testing.T) {
	withRaise := runBudgetBlockedMessage("a run for agent A", 45, 40, "multica issue budget raise CHE-1 --runs 55")
	if !strings.Contains(withRaise, "`multica issue budget raise CHE-1 --runs 55`") {
		t.Errorf("want the raise command in %q", withRaise)
	}
	for _, c := range []struct{ runs, max int }{{1000, 1000}, {1010, 1000}} {
		msg := runBudgetBlockedMessage("a run for agent A", c.runs, c.max, "")
		if strings.Contains(msg, "multica issue budget raise") || strings.Contains(msg, "--runs") {
			t.Errorf("%d of %d: message offers a raise that cannot work: %q", c.runs, c.max, msg)
		}
		if !strings.Contains(msg, "maximum") || !strings.Contains(msg, "status") {
			t.Errorf("%d of %d: want the maximum state and the status recovery in %q", c.runs, c.max, msg)
		}
	}
}
