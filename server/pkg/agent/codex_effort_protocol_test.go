package agent

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCodexEffortProtocolBoundary(test *testing.T) {
	if runtime.GOOS == "windows" {
		test.Skip("shell-script fixture is POSIX-only")
	}
	for _, resume := range []bool{false, true} {
		for _, level := range []string{"", "high"} {
			name := "fresh/"
			if resume {
				name = "resume/"
			}
			name += "effort=" + level
			test.Run(name, func(test *testing.T) {
				requests := filepath.Join(test.TempDir(), "requests.jsonl")
				fakePath := writeFakeCodexAppServer(test, ""+
					`read line; printf '%s\n' "$line" >> "$REQUESTS"`+"\n"+
					`echo '{"jsonrpc":"2.0","id":1,"result":{}}'`+"\n"+
					`read line; printf '%s\n' "$line" >> "$REQUESTS"`+"\n"+
					`read line; printf '%s\n' "$line" >> "$REQUESTS"`+"\n"+
					`echo '{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-effort","model":"gpt-6.1-sol","reasoningEffort":"xhigh"}}}'`+"\n"+
					`read line; printf '%s\n' "$line" >> "$REQUESTS"`+"\n"+
					`echo '{"jsonrpc":"2.0","id":3,"result":{}}'`+"\n"+
					`echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr-effort","turn":{"id":"turn-effort","status":"completed"}}}'`+"\n")
				opts := ExecOptions{Model: "gpt-6.1-sol", ThinkingLevel: level, Timeout: 5 * time.Second}
				setupMethod := "thread/start"
				if resume {
					opts.ResumeSessionID = "thr-effort"
					setupMethod = "thread/resume"
				}
				result, _ := executeFakeCodexCollectingMessagesWithConfig(test, fakePath,
					Config{Logger: slog.Default(), Env: map[string]string{
						"CODEX_HOME": test.TempDir(), "REQUESTS": requests,
					}}, opts, 10*time.Second)
				if result.Status != "completed" {
					test.Fatalf("status=%q error=%q", result.Status, result.Error)
				}
				file, err := os.Open(requests)
				if err != nil {
					test.Fatal(err)
				}
				defer file.Close()
				scanner := bufio.NewScanner(file)
				seen := map[string]int{}
				for scanner.Scan() {
					var request struct {
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
						test.Fatal(err)
					}
					test.Logf("request: %s", scanner.Text())
					seen[request.Method]++
					switch request.Method {
					case "thread/start", "thread/resume":
						if request.Method != setupMethod || request.Params["model"] != opts.Model {
							test.Fatalf("unexpected setup: %+v", request)
						}
						if request.Params["modelProvider"] != nil {
							test.Fatalf("unexpected provider override: %+v", request)
						}
						if resume && request.Params["threadId"] != opts.ResumeSessionID {
							test.Fatalf("unexpected resumed thread: %+v", request)
						}
						assertCodexThreadConfigEffort(test, request.Method, request.Params, level)
					case "turn/start":
						if request.Params["threadId"] != "thr-effort" {
							test.Fatalf("unexpected turn thread: %+v", request)
						}
						assertCodexTurnEffort(test, request.Method, request.Params, level)
					}
				}
				if err := scanner.Err(); err != nil {
					test.Fatal(err)
				}
				if seen[setupMethod] != 1 || seen["turn/start"] != 1 || seen["thread/start"]+seen["thread/resume"] != 1 {
					test.Fatalf("unexpected request counts: %v", seen)
				}
			})
		}
	}
}
