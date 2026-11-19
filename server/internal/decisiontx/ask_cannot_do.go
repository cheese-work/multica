package decisiontx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/multica-ai/multica/server/internal/guard"
)

// CHE-5902: an option that promises an agent action an agent run cannot do is
// refused or hinted at card creation. Only typed fields are read, never
// what_happens (CHE-3345): after_choice, agent_commands (argv arrays) and
// agent_surface, compared with the workspace catalog agent_cannot_do.

const (
	SettingCannotDoMode = "agent_cannot_do_mode"
	SettingCannotDo     = "agent_cannot_do"

	AfterAgentRun   = "agent_run"
	AfterHumanSteps = "human_steps"
	AfterRecordOnly = "record_only"
)

const argvExample = `["$MULTICA_CLI_PATH", "daemon", "restart"]`

// CannotDo is one catalog entry. A Tool entry has the tool guard's Match shape;
// an entry with no Tool is a console-only surface that an option names with agent_surface.
type CannotDo struct {
	Key        string   `json:"key"`
	Tool       string   `json:"tool,omitempty"`
	Subcommand []string `json:"subcommand,omitempty"`
	HumanStep  string   `json:"human_step"`
	// AllowedAgents (agent ids) may run a matching command; any other asker is refused.
	AllowedAgents []string `json:"allowed_agents,omitempty"`
}

// builtinCannotDo is the catalog of a workspace that sets no agent_cannot_do.
var builtinCannotDo = []CannotDo{
	{Key: "daemon_restart", Tool: "multica", Subcommand: []string{"daemon", "restart"}, HumanStep: "Cheese runs `multica daemon restart` on the daemon host; an agent run cannot restart its own daemon."},
	{Key: "daemon_stop", Tool: "multica", Subcommand: []string{"daemon", "stop"}, HumanStep: "Cheese runs `multica daemon stop` on the daemon host; an agent run cannot stop its own daemon."},
	// c00 seed: the doop restart of 18 Nov 2026 (CHE-7149 receipt) ran `docker container restart <id>` on C00, a command an X99 agent cannot run. `docker compose restart` is the same restart by another route.
	{Key: "c00_restart", Tool: "docker", Subcommand: []string{"container", "restart"}, HumanStep: "A C00 agent restarts the service; assign the issue to it, or Cheese runs the command on C00.", AllowedAgents: []string{c00HermesDevops}},
	{Key: "c00_compose_restart", Tool: "docker", Subcommand: []string{"compose", "restart"}, HumanStep: "A C00 agent restarts the service; assign the issue to it, or Cheese runs the command on C00.", AllowedAgents: []string{c00HermesDevops}},
}

// c00HermesDevops is the workspace agent that runs on C00 (c00-hermes-devops).
const c00HermesDevops = "b2b52f93-32e6-4caf-80ad-1b48cf60b821"

// hasSpace reports a space or control character: a canonical command token has none.
func hasSpace(token string) bool {
	return strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
}

var (
	catalogKey = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	agentID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// CannotDoFromSettings reads the mode (default off) and the catalog (default
// the built-in entries; an explicit list, even empty, replaces them) from a
// workspace.settings value. A malformed value is an error: callers must not guess.
func CannotDoFromSettings(raw []byte) (mode string, catalog []CannotDo, err error) {
	mode, catalog = "off", slices.Clone(builtinCannotDo)
	if len(raw) == 0 {
		return mode, catalog, nil
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", nil, fmt.Errorf("workspace settings: %w", err)
	}
	if value, ok := settings[SettingCannotDoMode]; ok {
		var set *string // null is not a mode
		if err := json.Unmarshal(value, &set); err != nil || set == nil || *set != "off" && *set != "warn" && *set != "block" {
			return "", nil, fmt.Errorf("%s must be off, warn or block", SettingCannotDoMode)
		}
		mode = *set
	}
	if value, ok := settings[SettingCannotDo]; ok {
		if catalog, err = ParseCannotDo(value); err != nil {
			return "", nil, err
		}
	}
	return mode, catalog, nil
}

// ParseCannotDo decodes and validates a catalog.
func ParseCannotDo(raw []byte) ([]CannotDo, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var catalog []CannotDo
	if err := decoder.Decode(&catalog); err != nil || catalog == nil {
		return nil, fmt.Errorf("%s must be a list of {key, tool, subcommand, human_step, allowed_agents}", SettingCannotDo)
	}
	seen := map[string]bool{}
	for index, entry := range catalog {
		field := fmt.Sprintf("%s[%d]", SettingCannotDo, index)
		switch {
		case !catalogKey.MatchString(entry.Key):
			return nil, fmt.Errorf("%s.key must be 1 to 40 characters of a-z, 0-9 and _", field)
		case seen[entry.Key]:
			return nil, fmt.Errorf("%s.key repeats %q", field, entry.Key)
		case strings.TrimSpace(entry.HumanStep) == "":
			return nil, fmt.Errorf("%s.human_step is required", field)
		case entry.Tool != strings.ToLower(entry.Tool) || strings.ContainsAny(entry.Tool, `/\`) || strings.HasSuffix(entry.Tool, ".exe"):
			return nil, fmt.Errorf("%s.tool must be a lower-case tool name, not a path", field)
		case entry.Tool == "" && len(entry.Subcommand) > 0:
			return nil, fmt.Errorf("%s.subcommand needs a tool", field)
		case hasSpace(entry.Tool):
			return nil, fmt.Errorf("%s.tool must not contain whitespace", field)
		}
		for _, word := range entry.Subcommand {
			if word == "" || hasSpace(word) || strings.HasPrefix(word, "-") {
				return nil, fmt.Errorf("%s.subcommand words must be non-empty, without whitespace, and not flags", field)
			}
		}
		agents := map[string]bool{}
		for _, id := range entry.AllowedAgents {
			if !agentID.MatchString(id) || agents[id] {
				return nil, fmt.Errorf("%s.allowed_agents must be distinct agent ids (lower-case UUIDs)", field)
			}
			agents[id] = true
		}
		seen[entry.Key] = true
	}
	return catalog, nil
}

// decodeArgvList reads agent_commands: a list of non-empty argv arrays of strings.
func decodeArgvList(field string, raw json.RawMessage, into *[][]string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return &AskError{field, "must be a list of argv arrays, for example [" + argvExample + "]"}
	}
	for index, item := range items {
		var argv []string
		if err := json.Unmarshal(item, &argv); err != nil || argv == nil {
			return &AskError{fmt.Sprintf("%s[%d]", field, index), "must be an argv array of strings, for example " + argvExample + "; the server parses no shell text"}
		}
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return &AskError{fmt.Sprintf("%s[%d]", field, index), "must not be empty"}
		}
		*into = append(*into, argv)
	}
	return nil
}

// validateAfterChoice checks the typed fields of one option, whatever the mode.
func (option AskOption) validateAfterChoice(prefix string) error {
	switch option.AfterChoice {
	case "", AfterHumanSteps, AfterRecordOnly, AfterAgentRun, AfterServerAction:
	default:
		return &AskError{prefix + ".after_choice", "must be agent_run, human_steps, record_only or server_action"}
	}
	if err := option.validateTyped(prefix); err != nil {
		return err
	}
	hasAgentFields := len(option.AgentCommands) > 0 || option.AgentSurface != ""
	switch {
	case option.AfterChoice == AfterAgentRun && !hasAgentFields:
		return &AskError{prefix + ".after_choice", "agent_run needs agent_commands or agent_surface"}
	case option.AfterChoice != "" && option.AfterChoice != AfterAgentRun && len(option.AgentCommands) > 0:
		return &AskError{prefix + ".agent_commands", "needs after_choice agent_run"}
	case option.AfterChoice != "" && option.AfterChoice != AfterAgentRun && option.AgentSurface != "":
		return &AskError{prefix + ".agent_surface", "needs after_choice agent_run"}
	}
	return nil
}

// hasWords reports that the command's leading words start with prefix.
func hasWords(words []string, prefix ...string) bool {
	return len(words) >= len(prefix) && slices.Equal(words[:len(prefix)], prefix)
}

// relayOnlyFix names the typed server_action that replaces the commands when
// each one only moves or assigns an issue (T2); "" when any command does more.
func relayOnlyFix(commands [][]string) string {
	status, assign := false, false
	for _, argv := range commands {
		tool, words, wrapper := guard.CanonicalCommand(argv)
		switch {
		case tool != "multica" || wrapper:
			return ""
		case hasWords(words, "issue", "status"):
			status = true
		case hasWords(words, "issue", "assign"):
			assign = true
		default:
			return ""
		}
	}
	switch {
	case status && assign:
		return "issue.status.set or issue.assign"
	case status:
		return "issue.status.set"
	case assign:
		return "issue.assign"
	}
	return ""
}

// CheckCannotDo compares the typed fields of each option with the catalog.
// Mode off checks nothing; warn returns hints; block returns the first refusal
// as an AskError with one line: the option label, the catalog key and the human step.
// asker is the asking agent's id ("" for a card with no agent): an entry with
// allowed_agents refuses every other asker.
func (ask Ask) CheckCannotDo(mode string, catalog []CannotDo, asker string) (hints []string, err error) {
	if mode == "off" {
		return nil, nil
	}
	report := func(field, problem string) error {
		if mode == "block" {
			return &AskError{field, problem}
		}
		hints = append(hints, field+" "+problem)
		return nil
	}
	missingReported := false
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		if option.AfterChoice == "" && !missingReported {
			missingReported = true // one hint per ask, not one per option
			if err := report(prefix+".after_choice", "is required: agent_run, human_steps or record_only"); err != nil {
				return nil, err
			}
		}
		if fix := relayOnlyFix(option.AgentCommands); fix != "" {
			if err := report(prefix, fmt.Sprintf("%q only moves an issue; send it as a typed option instead: after_choice server_action, options[].action %s", option.Label, fix)); err != nil {
				return nil, err
			}
		}
		for _, argv := range option.AgentCommands {
			tool, words, wrapper := guard.CanonicalCommand(argv)
			if tool == "multica" && !wrapper && (hasWords(words, "decision", "propose") || hasWords(words, "issue", "budget", "raise") || hasWords(words, "issue", "budget", "top-up")) {
				if err := report(prefix, fmt.Sprintf("%q needs a passkey card; attach the action with --action-file; the passkey goes on this card", option.Label)); err != nil {
					return nil, err
				}
				continue
			}
			if wrapper {
				if err := report(prefix, fmt.Sprintf("%q runs its command through %q; give the command itself as argv", option.Label, tool)); err != nil {
					return nil, err
				}
				continue
			}
			for _, entry := range catalog {
				if entry.Tool != "" && (guard.Match{Tool: entry.Tool, Subcommand: entry.Subcommand}).Covers(tool, words) && !slices.Contains(entry.AllowedAgents, asker) {
					if err := report(prefix, entry.refusal(option.Label)); err != nil {
						return nil, err
					}
				}
			}
		}
		for _, entry := range catalog {
			if option.AgentSurface != "" && entry.Key == option.AgentSurface {
				if err := report(prefix, entry.refusal(option.Label)); err != nil {
					return nil, err
				}
			}
		}
	}
	return hints, nil
}

func (entry CannotDo) refusal(label string) string {
	if len(entry.AllowedAgents) > 0 {
		return fmt.Sprintf("%q promises an agent action (%s) that only agents %s may run; set after_choice to human_steps and put this in human_steps: %s, or send options[].action issue.assign to one of them", label, entry.Key, strings.Join(entry.AllowedAgents, ", "), entry.HumanStep)
	}
	return fmt.Sprintf("%q promises an agent action (%s) that an agent run cannot do; set after_choice to human_steps and put this in human_steps: %s", label, entry.Key, entry.HumanStep)
}
