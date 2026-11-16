package guard

import (
	"slices"
	"strings"
)

// wrapperTools hide the command they run, so a match on them proves nothing.
var wrapperTools = []string{"env", "sudo", "nohup", "timeout", "xargs", "bash", "sh", "zsh"}

// multicaGlobalFlags can stand before the multica subcommand; the first three take a value.
var multicaGlobalFlags = map[string]bool{"--profile": true, "--server-url": true, "--workspace-id": true, "--debug": false}

// CanonicalCommand turns an argv array into the tool and the leading words
// after it, so "$MULTICA_CLI_PATH", /home/u/bin/multica and multica.exe are one
// tool (CHE-5902). wrapper is true when the tool is env, sudo, bash and the
// like: the real command is one of its arguments.
func CanonicalCommand(argv []string) (tool string, words []string, wrapper bool) {
	if len(argv) == 0 {
		return "", nil, false
	}
	first := argv[0]
	if n := len(first); n >= 2 && (first[0] == '"' || first[0] == '\'') && first[n-1] == first[0] {
		first = first[1 : n-1]
	}
	if first == "$MULTICA_CLI_PATH" || first == "${MULTICA_CLI_PATH}" {
		first = "multica"
	}
	tool = strings.ToLower(first[strings.LastIndexAny(first, `/\`)+1:])
	tool = strings.TrimSuffix(tool, ".exe")
	args := argv[1:]
	if tool == "multica" {
		args = dropMulticaGlobals(args)
	}
	return tool, leadingWords(args), slices.Contains(wrapperTools, tool)
}

func dropMulticaGlobals(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, _, _ := strings.Cut(args[i], "=")
		takesValue, global := multicaGlobalFlags[name]
		if !global {
			out = append(out, args[i])
			continue
		}
		if takesValue && !strings.Contains(args[i], "=") {
			i++ // the value
		}
	}
	return out
}

// Covers reports whether a canonical command is this match: the same tool and
// the match's subcommand as the first words, word by word.
func (m Match) Covers(tool string, words []string) bool {
	return m.Tool == tool && len(words) >= len(m.Subcommand) && slices.Equal(words[:len(m.Subcommand)], m.Subcommand)
}
