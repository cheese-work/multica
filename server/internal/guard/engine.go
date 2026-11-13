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
	Policy *Policy
	Runner Runner
	Log    io.Writer
	Out    io.Writer
	Err    io.Writer
}

// decision is what a plugin concluded about one call.
type decision struct {
	kind string
	line string   // the one guard line, without the "guard: " prefix
	code int      // exit code when done
	args []string // call to stream when not done
	done bool     // the plugin already ran the call and printed its output
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
}

// Exec runs tool with args under the policy and returns the exit code the
// caller should use.
func (e *Engine) Exec(ctx context.Context, tool string, args []string) (int, error) {
	args = e.expandAlias(ctx, tool, args)
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
	case pl.Action == ActionDeny:
		return refuse(2, "%s", pl.Message), nil
	}
	return pass(args), nil
}

// match returns the first plugin whose tool and leading words fit the call.
func (e *Engine) match(tool string, args []string) *Plugin {
	lead := leadingWords(args)
	for i := range e.Policy.Plugins {
		pl := &e.Policy.Plugins[i]
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
	b, err := json.Marshal(logRecord{Time: time.Now().UTC().Format(time.RFC3339), Plugin: pl.Name,
		Action: d.kind, Tool: tool, Sub: strings.Join(lead, " "), Line: d.line})
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
