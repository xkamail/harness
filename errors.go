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
	var r retryable
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return false
}
