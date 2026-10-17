// Package dbosharness makes a harness run durable with DBOS: every model
// call, tool call, inbox read and context read is a checkpointed step, so a
// run interrupted by a crash or restart resumes without repeating them.
//
// The project still owns the workflow function, its registration, the
// conversation store, run_id fencing and stop signals:
//
//	func runWorkflow(dctx dbos.DBOSContext, in runInput) (struct{}, error) {
//		wctx := dbos.From(dctx, rctx)
//		res, err := agents[in.Agent].Run(rctx,
//			dbosharness.Model(wctx, models.Get(in.Model), dbosharness.WithOnUsage(record)),
//			harness.RunConfig{
//				History: history,
//				Tools:   dbosharness.Tools(wctx),
//				Inbox:   dbosharness.Inbox(wctx, readQueued),
//			})
//		...
//	}
//
// Rules for workflow code: read nothing outside a step (history, context,
// config), and pass the chosen model in the workflow input, so a replay
// sees exactly what the first execution saw.
package dbosharness

import (
	"context"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/xkamail/harness"
)

// Option configures the step wrappers.
type Option func(*options)

type options struct {
	attempts  int
	backoff   time.Duration
	onUsage   func(ctx context.Context, m harness.Model, u harness.Usage)
	onCalls   func(calls []harness.ToolCall)
	onStepErr func(ctx context.Context, err error)
}

// WithRetry sets attempts and linear backoff for retryable model errors
// (harness.IsRetryable) inside the step; the default is 3 and 2s. Before
// each retry of a call that already streamed text it calls
// Request.OnReset, which Run turns into RunConfig.OnReset(messageID).
func WithRetry(attempts int, backoff time.Duration) Option {
	return func(o *options) { o.attempts, o.backoff = attempts, backoff }
}

// WithOnUsage is called inside the model step with the call's exact usage,
// so it runs once per call even across replays.
func WithOnUsage(f func(ctx context.Context, m harness.Model, u harness.Usage)) Option {
	return func(o *options) { o.onUsage = f }
}

// WithOnCalls hears every model turn's tool calls, on first execution and on
// replay alike, such as to rebuild grant state.
func WithOnCalls(f func(calls []harness.ToolCall)) Option {
	return func(o *options) { o.onCalls = f }
}

// WithOnStepError hears a step's final error, such as to park the run when
// the process is shutting down.
func WithOnStepError(f func(ctx context.Context, err error)) Option {
	return func(o *options) { o.onStepErr = f }
}

// Model wraps m so each Message is one step named "model". The checkpoint
// holds the neutral response, with tool arguments stored as strings.
func Model(wctx dbos.DBOSContext, m harness.Model, opts ...Option) harness.Model {
	// TODO: port stepLLM from mccloud api/ai/run.go.
	return nil
}

// Tools returns a ToolRunner that runs each call as its own concurrent step
// named "tool"; dbos.Go assigns step ids in call order, so a replay matches
// every call to its result.
func Tools(wctx dbos.DBOSContext, opts ...Option) harness.ToolRunner {
	// TODO: port stepTools from mccloud api/ai/run.go.
	return nil
}

// Inbox wraps read so each check is a step and a replay injects exactly the
// messages the first execution did.
func Inbox(wctx dbos.DBOSContext, read harness.Inbox) harness.Inbox {
	// TODO: port stepInbox from mccloud api/ai/run.go.
	return nil
}

// Context wraps an Agent.Context func as a step.
func Context(wctx dbos.DBOSContext, fn func(ctx context.Context) (string, error)) func(ctx context.Context) (string, error) {
	// TODO
	return nil
}

// OrphanSweeper fails runs a deploy stranded: DBOS recovers only workflows
// of its own application version.
type OrphanSweeper interface {
	// RunningIdle returns conversation and run ids marked running and idle
	// for at least idle.
	RunningIdle(ctx context.Context, idle time.Duration) (conversationIDs, runIDs []string, err error)
	// Fail marks the run failed when it is still the conversation's run.
	Fail(ctx context.Context, conversationID, runID string) error
}

// FailOrphanedRuns fails every run s reports whose workflow DBOS no longer
// knows or has ended. Call it after dbos.Launch with idle 0, then
// periodically.
func FailOrphanedRuns(ctx context.Context, dc dbos.DBOSContext, s OrphanSweeper, idle time.Duration) error {
	// TODO: port FailOrphanedRuns from mccloud api/ai/run.go.
	return harness.ErrNotImplemented
}
