package service

import (
	"context"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type enqueuedTaskHookKey struct{}

// WithEnqueuedTaskHook returns a context whose issue and mention task enqueues
// call fn with each task row they create. A caller that must tell the run its
// own action started from a later run of the same issue (the GitHub kill
// switch, CHE-2220) tags the exact row here instead of guessing afterward.
func WithEnqueuedTaskHook(ctx context.Context, fn func(db.AgentTaskQueue)) context.Context {
	return context.WithValue(ctx, enqueuedTaskHookKey{}, fn)
}

func notifyTaskEnqueued(ctx context.Context, task db.AgentTaskQueue) {
	if fn, ok := ctx.Value(enqueuedTaskHookKey{}).(func(db.AgentTaskQueue)); ok {
		fn(task)
	}
}

// deferEnqueuedTaskHook holds the caller's hook until the enqueue transaction
// commits: the hook reads the row through the pool, where an uncommitted insert
// is invisible. It returns the context to enqueue under and the call to make
// after commit.
func deferEnqueuedTaskHook(ctx context.Context) (context.Context, func()) {
	fn, ok := ctx.Value(enqueuedTaskHookKey{}).(func(db.AgentTaskQueue))
	if !ok {
		return ctx, func() {}
	}
	var tasks []db.AgentTaskQueue
	held := WithEnqueuedTaskHook(ctx, func(t db.AgentTaskQueue) { tasks = append(tasks, t) })
	return held, func() {
		for _, t := range tasks {
			fn(t)
		}
	}
}
