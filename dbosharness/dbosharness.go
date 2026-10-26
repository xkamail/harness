// Package dbosharness makes a harness run durable with DBOS: model calls, tool
// calls, inbox and context reads are checkpointed steps, so a resumed run never repeats them.
// Workflow code must read nothing outside a step and take the model from the workflow input, so replay sees what the first run saw.
package dbosharness

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"

	"github.com/xkamail/harness"
)

type Option func(*options)

type options struct {
	attempts  int
	backoff   time.Duration
	onUsage   func(ctx context.Context, m harness.Model, u harness.Usage)
	onCalls   func(calls []harness.ToolCall)
	onStepErr func(ctx context.Context, err error)
}

// WithRetry sets in-step retries for harness.IsRetryable errors (default 3, 2s linear backoff).
// A retry after streamed text calls Request.OnReset first.
func WithRetry(attempts int, backoff time.Duration) Option {
	return func(o *options) { o.attempts, o.backoff = attempts, backoff }
}

// WithOnUsage runs inside the model step, so once per call even across replays.
func WithOnUsage(f func(ctx context.Context, m harness.Model, u harness.Usage)) Option {
	return func(o *options) { o.onUsage = f }
}

// WithOnCalls sees each turn's tool calls on first execution and on replay (e.g. to rebuild grants).
func WithOnCalls(f func(calls []harness.ToolCall)) Option {
	return func(o *options) { o.onCalls = f }
}

// WithOnStepError sees a step's final error (e.g. to park the run on shutdown).
func WithOnStepError(f func(ctx context.Context, err error)) Option {
	return func(o *options) { o.onStepErr = f }
}

func newOptions(opts []Option) options {
	o := options{attempts: 3, backoff: 2 * time.Second}
	for _, f := range opts {
		f(&o)
	}
	return o
}

func (o options) stepErr(ctx context.Context, err error) {
	if o.onStepErr != nil {
		o.onStepErr(ctx, err)
	}
}

// Model wraps m so each Message is one step named "model".
// Message runs on wctx, not Run's ctx, so build wctx with dbos.From(dctx, rctx).
// A sub-agent's calls run inline on their own ctx: its parent's tool step checkpoints the reply.
func Model(wctx dbos.DBOSContext, m harness.Model, opts ...Option) harness.Model {
	return &stepModel{wctx: wctx, m: m, o: newOptions(opts)}
}

type stepModel struct {
	wctx dbos.DBOSContext
	m    harness.Model
	o    options
}

func (m *stepModel) Provider() string { return m.m.Provider() }
func (m *stepModel) Model() string    { return m.m.Model() }

// Arguments are strings: a json.RawMessage checkpoint would be re-encoded on replay.
type modelStep struct {
	Content   string        `json:"content"`
	Calls     []callStep    `json:"calls"`
	Usage     harness.Usage `json:"usage"`
	Reasoning string        `json:"reasoning,omitempty"`
}

type callStep struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (m *stepModel) Message(ctx context.Context, req harness.Request) (*harness.Response, error) {
	run := func(ctx context.Context) (modelStep, error) { return m.call(ctx, req) }
	sub := harness.IsSubAgent(ctx)
	var (
		out modelStep
		err error
	)
	if sub {
		out, err = run(ctx)
	} else {
		out, err = dbos.RunAsStep(m.wctx, run, dbos.WithStepName("model"))
	}
	if err != nil {
		return nil, err
	}
	msg := harness.Message{Role: harness.RoleAssistant, Content: out.Content}
	for _, c := range out.Calls {
		msg.ToolCalls = append(msg.ToolCalls, harness.ToolCall{ID: c.ID, Name: c.Name, Arguments: json.RawMessage(c.Arguments)})
	}
	// a step-limit turn's calls are never dispatched; a sub-agent's never re-run on replay
	if m.o.onCalls != nil && len(msg.ToolCalls) > 0 && !req.NoToolCalls && !sub {
		m.o.onCalls(msg.ToolCalls)
	}
	return &harness.Response{Message: msg, Usage: out.Usage, Reasoning: out.Reasoning}, nil
}

func (m *stepModel) call(ctx context.Context, req harness.Request) (modelStep, error) {
	// streamed is read only between attempts, after Message returned
	streamed := false
	inner := req
	if req.OnDelta != nil {
		inner.OnDelta = func(text string) { streamed = true; req.OnDelta(text) }
	}
	if req.OnReasoning != nil {
		inner.OnReasoning = func(text string) { streamed = true; req.OnReasoning(text) }
	}
	var (
		resp *harness.Response
		err  error
	)
	for attempt := 1; ; attempt++ {
		resp, err = m.m.Message(ctx, inner)
		if err == nil || attempt >= m.o.attempts || !harness.IsRetryable(err) {
			break
		}
		select {
		case <-ctx.Done():
			return modelStep{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * m.o.backoff):
		}
		if streamed && req.OnReset != nil {
			req.OnReset()
		}
		streamed = false
	}
	if err != nil {
		m.o.stepErr(ctx, err)
		return modelStep{}, err
	}
	if m.o.onUsage != nil {
		m.o.onUsage(ctx, m.m, resp.Usage)
	}
	st := modelStep{Content: resp.Message.Content, Usage: resp.Usage, Reasoning: resp.Reasoning}
	for _, c := range resp.Message.ToolCalls {
		st.Calls = append(st.Calls, callStep{ID: c.ID, Name: c.Name, Arguments: string(c.Arguments)})
	}
	return st, nil
}

// Tools runs each call as its own concurrent "tool" step; dbos.Go assigns step
// ids in call order, so replay matches each call to its result.
func Tools(wctx dbos.DBOSContext, opts ...Option) harness.ToolRunner {
	o := newOptions(opts)
	return func(_ context.Context, fns []func(context.Context) (string, error)) ([]string, error) {
		chs := make([]chan dbos.StepOutcome[string], len(fns))
		errs := make([]error, len(fns))
		for i, fn := range fns {
			chs[i], errs[i] = dbos.Go(wctx, func(ctx context.Context) (string, error) {
				out, err := fn(ctx)
				if err != nil {
					o.stepErr(ctx, err)
				}
				return out, err
			}, dbos.WithStepName("tool"))
		}
		outs := make([]string, len(fns))
		for i, ch := range chs {
			if ch == nil {
				continue
			}
			r := <-ch
			outs[i], errs[i] = r.Result, r.Err
		}
		return outs, errors.Join(errs...)
	}
}

type inboxStep struct {
	Msgs   []harness.Message `json:"msgs"`
	Revoke bool              `json:"revoke"`
}

// Inbox wraps read as a step so replay injects exactly the original messages.
func Inbox(wctx dbos.DBOSContext, read harness.Inbox) harness.Inbox {
	return func(context.Context) ([]harness.Message, bool, error) {
		out, err := dbos.RunAsStep(wctx, func(ctx context.Context) (inboxStep, error) {
			msgs, revoke, err := read(ctx)
			return inboxStep{Msgs: msgs, Revoke: revoke}, err
		}, dbos.WithStepName("inbox"))
		return out.Msgs, out.Revoke, err
	}
}

func Context(wctx dbos.DBOSContext, fn func(ctx context.Context) (string, error)) func(ctx context.Context) (string, error) {
	return func(context.Context) (string, error) {
		return dbos.RunAsStep(wctx, func(ctx context.Context) (string, error) {
			return fn(ctx)
		}, dbos.WithStepName("context"))
	}
}

// OrphanSweeper exists because DBOS recovers only workflows of its own application version.
type OrphanSweeper interface {
	RunningIdle(ctx context.Context, idle time.Duration) (conversationIDs, runIDs []string, err error)
	// Fail must be a no-op if runID is no longer the conversation's run.
	Fail(ctx context.Context, conversationID, runID string) error
}

// FailOrphanedRuns fails runs whose workflow is unknown, ended, or pending under another build.
// Call it after dbos.Launch with idle 0, then periodically.
func FailOrphanedRuns(ctx context.Context, dc dbos.DBOSContext, s OrphanSweeper, idle time.Duration) error {
	convs, runs, err := s.RunningIdle(ctx, idle)
	if err != nil || len(runs) == 0 {
		return err
	}
	live, err := dbos.ListWorkflows(dc,
		dbos.WithWorkflowIDs(runs),
		dbos.WithStatus([]dbos.WorkflowStatusType{dbos.WorkflowStatusPending, dbos.WorkflowStatusEnqueued}),
		dbos.WithLoadInput(false),
		dbos.WithLoadOutput(false),
	)
	if err != nil {
		return err
	}
	// pending under another build is stranded: recovery skips it
	pending := make(map[string]bool, len(live))
	for _, w := range live {
		pending[w.ID] = w.ApplicationVersion == dc.GetApplicationVersion()
	}
	var errs []error
	for i, runID := range runs {
		if pending[runID] {
			continue
		}
		if err := s.Fail(ctx, convs[i], runID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
