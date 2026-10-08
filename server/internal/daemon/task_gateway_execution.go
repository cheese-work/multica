package daemon

import (
	"context"
	"reflect"
	"strings"
	"sync"

	"github.com/multica-ai/multica/server/internal/taskgateway"
	"github.com/multica-ai/multica/server/pkg/agent"
	"github.com/multica-ai/multica/server/pkg/credentialexec"
)

type taskGatewayExecution struct {
	boundary  *credentialexec.Boundary
	backend   agent.Backend
	prompt    string
	options   agent.ExecOptions
	mutex     sync.Mutex
	closed    bool
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func (client *Client) prepareTaskGatewayExecution(ctx context.Context, task Task, spec credentialexec.Spec, options agent.ExecOptions) (*taskGatewayExecution, error) {
	if validateTaskIdentity(task) != nil || !task.RequireCredentialIsolation || task.Agent == nil || len(task.Agent.CustomArgs) != 0 || len(task.Agent.CustomEnv) != 0 || len(task.Agent.McpConfig) != 0 || len(task.Agent.RuntimeConfig) != 0 || len(task.Agent.DisabledRuntimeSkills) != 0 || spec.Provider != "claude" && spec.Provider != "codex" {
		return nil, taskgateway.ErrUnavailable
	}
	allowed := agent.ExecOptions{
		Timeout:                    options.Timeout,
		SemanticInactivityTimeout:  options.SemanticInactivityTimeout,
		FirstTurnNoProgressTimeout: options.FirstTurnNoProgressTimeout,
		HandshakeTimeout:           options.HandshakeTimeout,
		TurnInterruptTimeout:       options.TurnInterruptTimeout,
		ThreadHandshakeTimeout:     options.ThreadHandshakeTimeout,
		ResumeSessionID:            options.ResumeSessionID,
	}
	if !reflect.DeepEqual(options, allowed) || options.Timeout < 0 || options.SemanticInactivityTimeout < 0 || options.FirstTurnNoProgressTimeout < 0 || options.HandshakeTimeout < 0 || options.TurnInterruptTimeout < 0 || options.ThreadHandshakeTimeout < 0 || len(options.ResumeSessionID) > 256 || options.ResumeSessionID != task.PriorSessionID || task.PriorSessionResumeUnavailable {
		return nil, taskgateway.ErrUnavailable
	}
	for _, character := range options.ResumeSessionID {
		if character != '-' && character != '_' && (character < '0' || character > '9') && (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') {
			return nil, taskgateway.ErrUnavailable
		}
	}
	for _, value := range []string{task.Agent.Model, task.Agent.ThinkingLevel, task.Agent.ServiceTier} {
		if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, taskgateway.ErrUnavailable
		}
	}
	boundary, resolved, err := client.prepareTaskGateway(ctx, task, spec)
	if err != nil {
		return nil, taskgateway.ErrUnavailable
	}
	backend, err := agent.ResolveBackend(spec.Provider, agent.Config{
		ExecutablePath:             spec.Executable,
		TaskID:                     task.ID,
		RuntimeID:                  task.RuntimeID,
		BuiltinRuntime:             true,
		RequireCredentialIsolation: true,
		CredentialBoundary:         boundary,
	})
	if err != nil {
		_ = boundary.Close()
		return nil, taskgateway.ErrUnavailable
	}
	allowed.Cwd, allowed.Model = boundary.WorkDir(), task.Agent.Model
	allowed.ThinkingLevel, allowed.ServiceTier = task.Agent.ThinkingLevel, task.Agent.ServiceTier
	allowed.ResumeExpected = allowed.ResumeSessionID != ""
	return &taskGatewayExecution{boundary: boundary, backend: backend, prompt: BuildPrompt(resolved, spec.Provider), options: allowed}, nil
}

func (execution *taskGatewayExecution) Run(ctx context.Context, observe func(agent.Message)) (agent.Result, error) {
	if execution == nil {
		return agent.Result{}, taskgateway.ErrUnavailable
	}
	execution.mutex.Lock()
	if execution.closed || execution.cancel != nil {
		execution.mutex.Unlock()
		return agent.Result{}, taskgateway.ErrUnavailable
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	execution.cancel, execution.done = cancel, done
	execution.mutex.Unlock()
	defer func() {
		cancel()
		execution.mutex.Lock()
		execution.cancel, execution.done = nil, nil
		close(done)
		execution.mutex.Unlock()
	}()
	if execution.boundary.VerifyInputs(runCtx) != nil {
		return agent.Result{}, taskgateway.ErrUnavailable
	}
	session, err := execution.backend.Execute(runCtx, execution.prompt, execution.options)
	if err != nil {
		return agent.Result{}, taskgateway.ErrUnavailable
	}
	messages, results := session.Messages, session.Result
	var result agent.Result
	received := false
	for messages != nil || results != nil {
		select {
		case message, open := <-messages:
			if !open {
				messages = nil
			} else if observe != nil {
				observe(message)
			}
		case value, open := <-results:
			if open {
				result, received = value, true
			}
			results = nil
		}
	}
	if !received {
		return agent.Result{}, taskgateway.ErrUnavailable
	}
	return result, nil
}

func (execution *taskGatewayExecution) Close() error {
	if execution == nil {
		return nil
	}
	execution.closeOnce.Do(func() {
		execution.mutex.Lock()
		execution.closed = true
		cancel, done := execution.cancel, execution.done
		execution.mutex.Unlock()
		if cancel != nil {
			cancel()
			<-done
		}
		execution.closeErr = execution.boundary.Close()
	})
	return execution.closeErr
}
