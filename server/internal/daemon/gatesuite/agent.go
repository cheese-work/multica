package gatesuite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// AgentArg makes a test binary act as the fake agent: `<binary> __fake-agent
// <script.json>`. A package's TestMain calls Main first, so the same binary
// can run inside the sandbox, mounted like the A7c probe.
const AgentArg = "__fake-agent"

// Step is one action. Op is exec (Data is a shell script), read (Path),
// write (Path, Data), env (Name) or http (Method, URL, Header, Data).
type Step struct {
	Op, Path, Data, Name, Method, URL string
	Header                            map[string]string
}

// Outcome is what one step did. A refused step is recorded and the script
// goes on, so a test sees every refusal. An env step reports only "set" or
// "unset", never the value.
type Outcome struct {
	Op, Out, Err string
	OK           bool
	Status       int // http only
}

// Main runs the script named by args (os.Args[1:]) and prints one JSON
// Outcome per line; ok is false when args do not ask for the fake agent.
func Main(args []string) (code int, ok bool) {
	if len(args) != 2 || args[0] != AgentArg {
		return 0, false
	}
	raw, err := os.ReadFile(args[1])
	var steps []Step
	if err == nil {
		err = json.Unmarshal(raw, &steps)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake agent:", err)
		return 2, true
	}
	enc := json.NewEncoder(os.Stdout)
	for _, s := range steps {
		if err := enc.Encode(s.do()); err != nil {
			return 2, true
		}
	}
	return 0, true
}

func (s Step) do() Outcome {
	o := Outcome{Op: s.Op, OK: true}
	var err error
	switch s.Op {
	case "exec":
		var out []byte
		cmd := exec.Command("sh") // the script arrives on stdin, so no variable reaches the command line
		cmd.Stdin = strings.NewReader(s.Data)
		out, err = cmd.CombinedOutput()
		o.Out = strings.TrimSpace(string(out))
	case "read":
		var b []byte
		b, err = os.ReadFile(s.Path)
		o.Out = string(b)
	case "write":
		err = os.WriteFile(s.Path, []byte(s.Data), 0o644)
	case "env":
		o.Out = "unset"
		if _, set := os.LookupEnv(s.Name); set {
			o.Out = "set"
		}
	case "http":
		err = s.doHTTP(&o)
	default:
		err = fmt.Errorf("unknown op %q", s.Op)
	}
	if err != nil {
		o.OK, o.Err = false, err.Error()
	}
	return o
}

// doHTTP honours HTTPS_PROXY like a CLI does; status 400 and above fails the step.
func (s Step) doHTTP(o *Outcome) error {
	req, err := http.NewRequest(s.Method, s.URL, strings.NewReader(s.Data))
	if err != nil {
		return err
	}
	for k, v := range s.Header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	o.Status, o.Out = resp.StatusCode, string(b)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// WriteScript stores steps where AgentArg can read them.
func WriteScript(t testing.TB, steps []Step) string {
	t.Helper()
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/script.json"
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// RunAgent runs steps in the host, as a child of the test binary with extra
// env, and returns the outcomes in order.
func RunAgent(t testing.TB, steps []Step, env ...string) []Outcome {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the fake agent re-executes /proc/self/exe")
	}
	// A literal path: semgrep dangerous-exec-command flags a variable command.
	cmd := exec.Command("/proc/self/exe", AgentArg, WriteScript(t, steps))
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fake agent: %v: %s", err, stderr.String())
	}
	var out []Outcome
	for dec := json.NewDecoder(&stdout); dec.More(); {
		var o Outcome
		if err := dec.Decode(&o); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}
