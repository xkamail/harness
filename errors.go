package harness

import "errors"

// Sentinels. A run that ends on one still returns a non-nil *RunResult.
var (
	// ErrMaxIterations: the last call still asked for tools.
	ErrMaxIterations = errors.New("harness: max iterations reached")
	// ErrNoFinalReply: a turn with no text and no tool calls.
	ErrNoFinalReply = errors.New("harness: no final reply")
	// ErrBudgetExceeded: the run's Budget was spent.
	ErrBudgetExceeded = errors.New("harness: budget exceeded")
	// ErrNotCompiled: Run on an Agent that did not come from Compile.
	ErrNotCompiled = errors.New("harness: agent not compiled")
	// ErrStaleApproval: a decision names a confirmation or plan that is no
	// longer the conversation's open one.
	ErrStaleApproval = errors.New("harness: approval is not open")
	// ErrNotImplemented marks the draft's unported bodies.
	ErrNotImplemented = errors.New("harness: not implemented")
)

// retryable is implemented by adapter errors that know their class.
type retryable interface {
	error
	Retryable() bool
}

// IsRetryable reports whether err is worth retrying the same model call:
// rate limits, 5xx and network errors are; bad requests, auth and harness
// sentinels are not. dbosharness uses the same rule for step retries.
func IsRetryable(err error) bool {
	var r retryable
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return false
}
