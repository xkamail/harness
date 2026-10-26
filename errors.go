package harness

import "errors"

// Sentinels. A run that ends on one still returns a non-nil *RunResult.
var (
	ErrMaxIterations = errors.New("harness: max iterations reached")
	// ErrNoFinalReply: a turn with no text and no tool calls.
	ErrNoFinalReply   = errors.New("harness: no final reply")
	ErrBudgetExceeded = errors.New("harness: budget exceeded")
	ErrNotCompiled    = errors.New("harness: agent not compiled")
	// ErrStaleApproval: the decision targets a confirmation or plan that is no longer open.
	ErrStaleApproval = errors.New("harness: approval is not open")
)

type retryable interface {
	error
	Retryable() bool
}

// IsRetryable reports whether a model call error is transient (rate limit,
// 5xx, network); errors not implementing Retryable() are not.
func IsRetryable(err error) bool {
	if r, ok := errors.AsType[retryable](err); ok {
		return r.Retryable()
	}
	return false
}

type contextLength interface {
	error
	ContextLength() bool
}

// IsContextLength reports whether a model call error says the request
// overflowed the model's context window; errors not implementing
// ContextLength() are not.
func IsContextLength(err error) bool {
	if c, ok := errors.AsType[contextLength](err); ok {
		return c.ContextLength()
	}
	return false
}
