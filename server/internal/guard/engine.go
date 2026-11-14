package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Engine checks one tool call against the policy, then runs it. Out and Err are
// the caller's stdout and stderr; Log, when set, receives one JSON line per
// guarded call.
type Engine struct {
	// Policy may carry org, repo and path layers; Exec resolves it for the
	// call's target. Read it through pol().
	Policy *Policy
	Runner Runner
	Log    io.Writer
	Out    io.Writer
	Err    io.Writer

	eff  *Policy // Policy resolved for the current call
	path string  // the call's directory inside its repository
}

func (e *Engine) pol() *Policy {
	if e.eff != nil {
		return e.eff
	}
	return e.Policy
}

// target tells which repository a call acts on and, when a path layer needs it,
// the directory inside it. The repository is the -R/--repo flag, else the repo
// of a PR URL given to `gh pr <verb>`, else the working directory's. ok is
// false when a required fact cannot be learned; the caller refuses the call.
func (e *Engine) target(ctx context.Context, args []string) (t Target, ok bool, why string) {
	cwdRepo := ""
	cwd := func() bool {
		if cwdRepo != "" {
			return true
		}
		var rv struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if err := e.ghJSON(ctx, &rv, "repo", "view", "--json", "nameWithOwner"); err != nil || rv.NameWithOwner == "" {
			return false
		}
		cwdRepo = rv.NameWithOwner
		return true
	}
	for i, a := range args {
		switch {
		case (a == "-R" || a == "--repo") && i+1 < len(args):
			t.Repo = args[i+1]
		case strings.HasPrefix(a, "--repo="):
			t.Repo = strings.TrimPrefix(a, "--repo=")
		}
	}
	if t.Repo == "" && len(args) > 0 && args[0] == "pr" {
		if _, pos := prSpec.scan(args); len(pos) > 2 {
			if m := prRepoRe.FindStringSubmatch(pos[2]); m != nil {
				t.Repo = m[1]
			}
		}
	}
	if t.Repo == "" {
		if !cwd() {
			return t, false, "cannot tell which repository this call targets; run it inside a clone or pass -R owner/name"
		}
		t.Repo = cwdRepo
	}
	if !e.Policy.hasPathLayer(t.Repo) {
		return t, true, ""
	}
	if !cwd() || !strings.EqualFold(cwdRepo, t.Repo) {
		return t, false, "the policy has path layers for " + t.Repo + " and this call runs outside it, so the directory cannot be checked; run it from a clone of " + t.Repo
	}
	prefix, ok := e.git(ctx, "rev-parse", "--show-prefix")
	if !ok {
		return t, false, "the policy has path layers for " + t.Repo + " and git cannot tell the working directory inside it"
	}
	t.Path = strings.TrimSuffix(prefix, "/")
	return t, true, ""
}

// decision is what a plugin concluded about one call.
type decision struct {
	kind  string
	line  string   // the one guard line, without the "guard: " prefix
	code  int      // exit code when done
	args  []string // call to stream when not done
	done  bool     // the plugin already ran the call and printed its output
	issue string   // issue key the call names, for the log
	sub   string   // command to log when the leading words are not enough
	note  string   // log-only text; never printed to the agent
}

func pass(args []string) decision { return decision{kind: KindPass, args: args} }

func refuse(code int, format string, a ...any) decision {
	return decision{kind: KindRefused, code: code, done: true, line: fmt.Sprintf(format, a...)}
}

type logRecord struct {
	Time   string `json:"time"`
	Plugin string `json:"plugin"`
	Action string `json:"action"`
	Tool   string `json:"tool"`
	Sub    string `json:"subcommand"`
	Line   string `json:"line"`
	Issue  string `json:"issue,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Exec runs tool with args under the policy and returns the exit code the
// caller should use.
func (e *Engine) Exec(ctx context.Context, tool string, args []string) (int, error) {
	if e.Policy.lockdown {
		fmt.Fprintf(e.Err, "guard: refused: %s\n", lockdownMessage)
		return 2, nil
	}
	args = e.expandAlias(ctx, tool, args)
	t := Target{}
	// ponytail: one extra gh repo view and git call per guarded call when the
	// policy has layers; cache per run if that shows up.
	if len(e.Policy.Layers) > 0 {
		var ok bool
		var why string
		if t, ok, why = e.target(ctx, args); !ok {
			fmt.Fprintln(e.Err, "guard: refused: "+why)
			return 2, nil
		}
	}
	e.eff, e.path = e.Policy.Resolve(t), t.Path
	pl := e.match(tool, args)
	if pl == nil {
		return e.Runner.Stream(ctx, tool, args...)
	}

	d, err := e.decide(ctx, pl, args)
	if err != nil {
		d = e.pluginError(pl, args, err)
	}
	if d.line != "" {
		fmt.Fprintln(e.Err, "guard: "+d.line)
	}
	code := d.code
	if !d.done {
		if code, err = e.Runner.Stream(ctx, tool, d.args...); err != nil {
			return 0, err
		}
	}
	e.log(pl, tool, args, d)
	return code, nil
}

// pluginError applies the plugin's failure mode: open runs the call unchecked
// with one warning; fail_closed refuses it.
func (e *Engine) pluginError(pl *Plugin, args []string, err error) decision {
	msg := firstLine(err.Error(), 200)
	if pl.FailClosed {
		return decision{kind: KindPluginError, code: 2, done: true,
			line: fmt.Sprintf("%s could not check this call (%s); refused because the plugin fails closed", pl.Name, msg)}
	}
	return decision{kind: KindPluginError, args: args,
		line: fmt.Sprintf("%s could not check this call (%s); running it unchecked", pl.Name, msg)}
}

func (e *Engine) decide(ctx context.Context, pl *Plugin, args []string) (decision, error) {
	switch {
	case pl.Action == ActionPass:
		return pass(args), nil
	case pl.Builtin == "gh-pr":
		return e.ghPR(ctx, pl, args)
	case pl.Builtin == "gh-api":
		return ghAPI(args), nil
	case pl.Builtin == "multica-wakeup":
		return e.wakeupShadow(ctx, args)
	case pl.Action == ActionDeny:
		return refuse(2, "%s", pl.Message), nil
	}
	return pass(args), nil
}

// match returns the first plugin whose tool and leading words fit the call.
func (e *Engine) match(tool string, args []string) *Plugin {
	lead := leadingWords(args)
	for i := range e.pol().Plugins {
		pl := &e.pol().Plugins[i]
		sub := pl.Match.Subcommand
		if pl.Match.Tool != tool || len(sub) > len(lead) {
			continue
		}
		if strings.Join(sub, " ") == strings.Join(lead[:len(sub)], " ") {
			return pl
		}
	}
	return nil
}

func (e *Engine) log(pl *Plugin, tool string, args []string, d decision) {
	if e.Log == nil {
		return
	}
	lead := leadingWords(args)
	if len(lead) > 2 {
		lead = lead[:2]
	}
	sub := strings.Join(lead, " ")
	if d.sub != "" {
		sub = d.sub
	}
	b, err := json.Marshal(logRecord{Time: time.Now().UTC().Format(time.RFC3339), Plugin: pl.Name,
		Action: d.kind, Tool: tool, Sub: sub, Line: d.line, Issue: d.issue, Note: d.note})
	if err == nil {
		fmt.Fprintf(e.Log, "%s\n", b)
	}
}

// ghCore lists the built-in gh commands; an alias cannot shadow them, so only
// other first words are looked up.
var ghCore = set("agent-task", "alias", "api", "attestation", "auth", "browse", "cache", "codespace", "completion", "config",
	"copilot", "extension", "gist", "gpg-key", "help", "issue", "label", "licenses", "org", "pr", "project", "release",
	"repo", "ruleset", "run", "search", "secret", "ssh-key", "status", "variable", "version", "workflow")

// expandAlias returns the call a gh alias stands for, so an alias cannot dodge
// the guard. A shell alias ("!...") and a failed lookup leave the call as is.
func (e *Engine) expandAlias(ctx context.Context, tool string, args []string) []string {
	if tool != "gh" || len(args) == 0 || strings.HasPrefix(args[0], "-") || ghCore[args[0]] {
		return args
	}
	res, err := e.Runner.Run(ctx, "gh", "alias", "list")
	if err != nil || res.Exit != 0 {
		return args
	}
	for _, l := range strings.Split(res.Stdout, "\n") {
		name, value, ok := strings.Cut(l, ":")
		if !ok || strings.TrimSpace(name) != args[0] {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "!") {
			return args
		}
		rest, used := args[1:], 0
		for i, a := range rest {
			if tok := "$" + strconv.Itoa(i+1); strings.Contains(value, tok) {
				value, used = strings.ReplaceAll(value, tok, a), i+1
			}
		}
		return append(strings.Fields(value), rest[used:]...)
	}
	return args
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}
