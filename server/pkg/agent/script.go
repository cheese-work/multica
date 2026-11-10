package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/redact"
)

// Script mode (CHE-4678 S9 P4): an autopilot tick runs one allowlisted command
// under the task token, with no model session. The daemon owns the allowlist
// (<Dir>/<name>.json); nothing the server sends can name an executable.
//
// Failure reasons ride as the prefix of Result.Error ("<reason>: <detail>");
// ScriptFailureReason reads them back.
const (
	ScriptReasonNotAllowlisted = "script_not_allowlisted"
	ScriptReasonArgsRejected   = "script_args_rejected"
	ScriptReasonFailed         = "script_failed"

	// ScriptDefaultTimeout applies when ScriptSpec.Timeout is unset. It sits far
	// below the 24 h task-token cap.
	ScriptDefaultTimeout = 10 * time.Minute

	scriptTailBytes   = 2048  // output tail returned in Result
	scriptBufferBytes = 16384 // raw tail kept for redaction, 8x the returned tail
	scriptEntryMax    = 64 << 10
)

// scriptNamePattern mirrors the server's script_config.name rule. It is checked
// again here because the name becomes a file path under the allowlist dir.
var scriptNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// scriptTokenPattern catches token-shaped strings the exact-value replacement
// cannot know about: task, daemon and personal access tokens.
var scriptTokenPattern = regexp.MustCompile(`\b(?:mat|mdt|mul)_[A-Za-z0-9_-]{6,}`)

// ScriptSpec selects the script backend. Dir is the daemon-local allowlist
// directory; an empty or missing Dir allowlists nothing.
type ScriptSpec struct {
	Dir     string
	Name    string
	Args    []string
	Timeout time.Duration
}

// scriptEntry is the on-disk allowlist entry. arg_patterns are full-match
// regular expressions, one of which every argument must satisfy; an entry with
// none accepts no arguments.
type scriptEntry struct {
	Executable  string   `json:"executable"`
	ArgPatterns []string `json:"arg_patterns"`
}

// ScriptFailureReason returns the machine reason prefixed to a script Result.Error.
func ScriptFailureReason(errText string) string {
	reason, _, _ := strings.Cut(errText, ":")
	switch reason {
	case ScriptReasonNotAllowlisted, ScriptReasonArgsRejected, ScriptReasonFailed:
		return reason
	}
	return ""
}

// scriptExitPattern reads the "script_failed: exit code N" line the backend writes.
var scriptExitPattern = regexp.MustCompile(`^` + ScriptReasonFailed + `: exit code (\d+)`)

// ScriptExitCode returns the exit code of a script_failed Result.Error. ok is
// false when the script never started or was rejected, timed out or cancelled.
func ScriptExitCode(errText string) (code int, ok bool) {
	m := scriptExitPattern.FindStringSubmatch(errText)
	if m == nil {
		return 0, false
	}
	code, err := strconv.Atoi(m[1])
	return code, err == nil
}

type scriptBackend struct{ cfg Config }

func newScriptBackend(cfg Config) Backend { return &scriptBackend{cfg: cfg} }

func (b *scriptBackend) Execute(ctx context.Context, _ string, opts ExecOptions) (*Session, error) {
	msgs := make(chan Message)
	close(msgs) // no model, so no transcript
	res := make(chan Result, 1)
	go func() {
		start := time.Now()
		r := b.run(ctx, opts.Cwd)
		r.DurationMs = time.Since(start).Milliseconds()
		res <- r
	}()
	return &Session{Messages: msgs, Result: res}, nil
}

func (b *scriptBackend) run(ctx context.Context, cwd string) Result {
	spec := b.cfg.Script
	log := b.cfg.Logger
	entry, err := loadScriptEntry(spec.Dir, spec.Name)
	if err != nil {
		log.Warn("script rejected", "task_id", b.cfg.TaskID, "reason", ScriptReasonNotAllowlisted, "script", spec.Name)
		return Result{Status: "failed", Error: ScriptReasonNotAllowlisted + ": " + err.Error()}
	}
	if err := entry.checkArgs(spec.Args); err != nil {
		log.Warn("script rejected", "task_id", b.cfg.TaskID, "reason", ScriptReasonArgsRejected, "script", spec.Name)
		return Result{Status: "failed", Error: ScriptReasonArgsRejected + ": " + err.Error()}
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = ScriptDefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No shell: argv goes straight to execve. The environment is the task's own
	// variables plus PATH, never the daemon's.
	cmd := newRuntimeCmd(exec.CommandContext(runCtx, entry.Executable, spec.Args...))
	cmd.Dir = cwd
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, envList(b.cfg.Env)...)
	tail := &tailWriter{max: scriptBufferBytes}
	cmd.Stdout, cmd.Stderr = tail, tail

	log.Info("script start", "task_id", b.cfg.TaskID, "script", spec.Name, "args", len(spec.Args), "timeout", timeout)
	runErr := runOwned(cmd, log)
	out := b.sanitize(tail.String())

	switch {
	case runErr == nil:
		log.Info("script completed", "task_id", b.cfg.TaskID, "script", spec.Name)
		return Result{Status: "completed", Output: out}
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		log.Warn("script timed out", "task_id", b.cfg.TaskID, "script", spec.Name, "timeout", timeout)
		return Result{Status: "timeout", Output: out, Error: fmt.Sprintf("script %q timed out after %s", spec.Name, timeout)}
	case ctx.Err() != nil:
		return Result{Status: "cancelled", Output: out, Error: "script cancelled"}
	}
	var exit *exec.ExitError
	reason := "did not start"
	if errors.As(runErr, &exit) {
		reason = fmt.Sprintf("exit code %d", exit.ExitCode())
	}
	log.Warn("script failed", "task_id", b.cfg.TaskID, "script", spec.Name, "detail", reason)
	return Result{Status: "failed", Output: out, Error: ScriptReasonFailed + ": " + reason + "\n" + out}
}

// sanitize redacts first, then caps: redacting the 16 KiB buffer before taking
// the last 2 KiB means a token cut at the buffer's start is dropped with it.
func (b *scriptBackend) sanitize(raw string) string {
	for _, v := range b.cfg.Env {
		if len(v) >= 8 && strings.HasPrefix(v, "mat_") {
			raw = strings.ReplaceAll(raw, v, "[REDACTED]")
		}
	}
	s := redact.Text(scriptTokenPattern.ReplaceAllString(raw, "[REDACTED]"))
	if len(s) > scriptTailBytes {
		s = s[len(s)-scriptTailBytes:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(s, ""))
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func loadScriptEntry(dir, name string) (*scriptEntry, error) {
	if dir == "" {
		return nil, errors.New("no script allowlist configured")
	}
	if !scriptNamePattern.MatchString(name) {
		return nil, errors.New("script name is not allowlisted")
	}
	raw, err := readCapped(filepath.Join(dir, name+".json"), scriptEntryMax)
	if err != nil {
		return nil, errors.New("script name is not allowlisted")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e scriptEntry
	if err := dec.Decode(&e); err != nil || !filepath.IsAbs(e.Executable) {
		return nil, errors.New("allowlist entry is invalid")
	}
	for _, p := range e.ArgPatterns {
		if _, err := regexp.Compile(`^(?:` + p + `)$`); err != nil {
			return nil, errors.New("allowlist entry is invalid")
		}
	}
	return &e, nil
}

func (e *scriptEntry) checkArgs(args []string) error {
	for i, a := range args {
		ok := false
		for _, p := range e.ArgPatterns {
			if regexp.MustCompile(`^(?:` + p + `)$`).MatchString(a) { // validated in loadScriptEntry
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("argument %d does not match an allowed pattern", i+1)
		}
	}
	return nil
}

func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("unreadable or oversized")
	}
	return b, nil
}

// tailWriter keeps only the last max bytes written to it.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.buf) }
