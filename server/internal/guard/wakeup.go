package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// wakeupSpec lists the valued flags of `multica issue wakeup create|update`;
// the rest are booleans.
var wakeupSpec = flagSpec{valued: set("after", "agent-id", "at", "cron", "decision", "event", "expires-at", "expires-in",
	"filter-actor-id", "filter-actor-type", "filter-agent-id", "instruction", "instruction-file", "kind", "max-fires",
	"mode", "on-timeout", "output", "parent", "purpose", "stage", "task-id", "timezone", "until-assignee",
	"until-gh-run", "until-gh-run-state", "until-issue", "until-issue-state", "until-label", "until-pr",
	"until-property", "until-status")}

// wakeupShadow is the shadow-mode check for a wait on a person (CHE-5377): a
// wakeup rule filtered to a member needs an open decision request for that
// member, created after the newest answered one on the issue. A miss is logged
// and never refused or printed; the call always runs. Only typed flags decide.
func (e *Engine) wakeupShadow(ctx context.Context, args []string) (decision, error) {
	flags, pos := wakeupSpec.scan(args)
	member, _ := find(flags, "filter-actor-id")
	if t, ok := find(flags, "filter-actor-type"); !ok || t.value != "member" || len(pos) < 4 {
		return pass(args), nil
	}
	d := decision{kind: KindPass, args: args, issue: pos[3], sub: strings.Join(pos[:3], " ")}
	res, err := e.Runner.Run(ctx, "multica", "decision", "list", "--issue", pos[3], "--output", "json")
	if err == nil && res.Exit != 0 {
		err = fmt.Errorf("decision list exited %d: %s", res.Exit, firstLine(res.Stderr, 100))
	}
	var rows []struct {
		Status  string `json:"status"`
		Created string `json:"created_at"`
		Human   string `json:"human_user_id"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(res.Stdout), &rows)
	}
	if err != nil {
		d.kind, d.note = KindPluginError, firstLine(err.Error(), 200)
		return d, nil
	}
	// RFC 3339 UTC strings of one issue sort in time order.
	var answered string
	for _, r := range rows {
		if r.Status == "answered" && r.Created > answered {
			answered = r.Created
		}
	}
	for _, r := range rows {
		if r.Status == "open" && r.Human == member.value && r.Created > answered {
			return d, nil
		}
	}
	d.kind = KindShadowHit
	d.note = "wait on a member with no open decision request after the last answered one"
	return d, nil
}
